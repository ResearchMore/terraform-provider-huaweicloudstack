// This set of code handles all functions required to configure networking
// on an huaweicloudstack_compute_instance_v2 resource.
//
// This is a complicated task because it's not possible to obtain all
// information in a single API call. In fact, it even traverses multiple
// HuaweiCloudStack services.
//
// The end result, from the user's point of view, is a structured set of
// understandable network information within the instance resource.
package huaweicloudstack

import (
	"fmt"
	"log"
	"os"

	"github.com/hashicorp/terraform/helper/schema"
	"github.com/huaweicloud/golangsdk"
	"github.com/huaweicloud/golangsdk/openstack/compute/v2/extensions/tenantnetworks"
	"github.com/huaweicloud/golangsdk/openstack/compute/v2/servers"
	"github.com/huaweicloud/golangsdk/openstack/networking/v2/networks"
	"github.com/huaweicloud/golangsdk/openstack/networking/v2/ports"
)

// InstanceNIC is a structured representation of a Gophercloud servers.Server
// virtual NIC.
type InstanceNIC struct {
	FixedIPv4 string
	FixedIPv6 string
	MAC       string
}

// InstanceAddresses is a collection of InstanceNICs, grouped by the
// network name. An instance/server could have multiple NICs on the same
// network.
type InstanceAddresses struct {
	NetworkName  string
	InstanceNICs []InstanceNIC
}

// InstanceNetwork represents a collection of network information that a
// Terraform instance needs to satisfy all network information requirements.
type InstanceNetwork struct {
	UUID          string
	Name          string
	Port          string
	FixedIP       string
	AccessNetwork bool
}

// getAllInstanceNetworks loops through the networks defined in the Terraform
// configuration and structures that information into something standard that
// can be consumed by both HuaweiCloudStack and Terraform.
//
// This would be simple, except we have ensure both the network name and
// network ID have been determined. This isn't just for the convenience of a
// user specifying a human-readable network name, but the network information
// returned by an HuaweiCloudStack instance only has the network name set! So if a
// user specified a network ID, there's no way to correlate it to the instance
// unless we know both the name and ID.
//
// Not only that, but we have to account for two HuaweiCloudStack network services
// running: nova-network (legacy) and Neutron (current).
//
// In addition, if a port was specified, not all of the port information
// will be displayed, such as multiple fixed and floating IPs. This resource
// isn't currently configured for that type of flexibility. It's better to
// reference the actual port resource itself.
//
// So, let's begin the journey.
func getAllInstanceNetworks(d *schema.ResourceData, meta interface{}) ([]InstanceNetwork, error) {
	var instanceNetworks []InstanceNetwork

	networks := d.Get("network").([]interface{})
	for _, v := range networks {
		network := v.(map[string]interface{})
		networkID := network["uuid"].(string)
		networkName := network["name"].(string)
		portID := network["port"].(string)

		if networkID == "" && networkName == "" && portID == "" {
			return nil, fmt.Errorf(
				"At least one of network.uuid, network.name, or network.port must be set.")
		}

		// If a user specified both an ID and name, that makes things easy
		// since both name and ID are already satisfied. No need to query
		// further.
		if networkID != "" && networkName != "" {
			v := InstanceNetwork{
				UUID:          networkID,
				Name:          networkName,
				Port:          portID,
				FixedIP:       network["fixed_ip_v4"].(string),
				AccessNetwork: network["access_network"].(bool),
			}
			instanceNetworks = append(instanceNetworks, v)
			continue
		}

		// But if at least one of name or ID was missing, we have to query
		// for that other piece.
		//
		// Priority is given to a port since a network ID or name usually isn't
		// specified when using a port.
		//
		// Next priority is given to the network ID since it's guaranteed to be
		// an exact match.
		queryType := "name"
		queryTerm := networkName
		if networkID != "" {
			queryType = "id"
			queryTerm = networkID
		}
		if portID != "" {
			queryType = "port"
			queryTerm = portID
		}

		networkInfo, err := getInstanceNetworkInfo(d, meta, queryType, queryTerm)
		if err != nil {
			return nil, err
		}

		v := InstanceNetwork{
			UUID:          networkInfo["uuid"].(string),
			Name:          networkInfo["name"].(string),
			Port:          portID,
			FixedIP:       network["fixed_ip_v4"].(string),
			AccessNetwork: network["access_network"].(bool),
		}

		instanceNetworks = append(instanceNetworks, v)
	}

	log.Printf("[DEBUG] getAllInstanceNetworks: %#v", instanceNetworks)
	return instanceNetworks, nil
}

// getInstanceNetworkInfo will query for network information in order to make
// an accurate determination of a network's name and a network's ID.
//
// We will try to first query the Neutron network service and fall back to the
// legacy nova-network service if that fails.
//
// If OS_NOVA_NETWORK is set, query nova-network even if Neutron is available.
// This is to be able to explicitly test the nova-network API.
func getInstanceNetworkInfo(
	d *schema.ResourceData, meta interface{}, queryType, queryTerm string) (map[string]interface{}, error) {

	config := meta.(*Config)

	if _, ok := os.LookupEnv("OS_NOVA_NETWORK"); !ok {
		networkClient, err := config.networkingV2Client(GetRegion(d, config))
		if err == nil {
			networkInfo, err := getInstanceNetworkInfoNeutron(networkClient, queryType, queryTerm)
			if err != nil {
				return nil, fmt.Errorf("Error trying to get network information from the Network API: %s", err)
			}

			return networkInfo, nil
		}
	}

	log.Printf("[DEBUG] Unable to obtain a network client")

	computeClient, err := config.computeV2Client(GetRegion(d, config))
	if err != nil {
		return nil, fmt.Errorf("Error creating HuaweiCloudStack compute client: %s", err)
	}

	networkInfo, err := getInstanceNetworkInfoNovaNet(computeClient, queryType, queryTerm)
	if err != nil {
		return nil, fmt.Errorf("Error trying to get network information from the Nova API: %s", err)
	}

	return networkInfo, nil
}

// getInstanceNetworkInfoNovaNet will query the os-tenant-networks API for
// the network information.
func getInstanceNetworkInfoNovaNet(
	client *golangsdk.ServiceClient, queryType, queryTerm string) (map[string]interface{}, error) {

	// If somehow a port ended up here, we should just error out.
	if queryType == "port" {
		return nil, fmt.Errorf(
			"Unable to query a port (%s) using the Nova API", queryTerm)
	}

	allPages, err := tenantnetworks.List(client).AllPages()
	if err != nil {
		return nil, fmt.Errorf(
			"An error occured while querying the Nova API for network information: %s", err)
	}

	networkList, err := tenantnetworks.ExtractNetworks(allPages)
	if err != nil {
		return nil, fmt.Errorf(
			"An error occured while querying the Nova API for network information: %s", err)
	}

	var networkFound bool
	var network tenantnetworks.Network

	for _, v := range networkList {
		if queryType == "id" && v.ID == queryTerm {
			networkFound = true
			network = v
			break
		}

		if queryType == "name" && v.Name == queryTerm {
			networkFound = true
			network = v
			break
		}
	}

	if networkFound {
		v := map[string]interface{}{
			"uuid": network.ID,
			"name": network.Name,
		}

		log.Printf("[DEBUG] getInstanceNetworkInfoNovaNet: %#v", v)
		return v, nil
	}

	return nil, fmt.Errorf("Could not find any matching network for %s %s", queryType, queryTerm)
}

// getInstanceNetworkInfoNeutron will query the neutron API for the network
// information.
func getInstanceNetworkInfoNeutron(
	client *golangsdk.ServiceClient, queryType, queryTerm string) (map[string]interface{}, error) {

	// If a port was specified, use it to look up the network ID
	// and then query the network as if a network ID was originally used.
	if queryType == "port" {
		listOpts := ports.ListOpts{
			ID: queryTerm,
		}
		allPages, err := ports.List(client, listOpts).AllPages()
		if err != nil {
			return nil, fmt.Errorf("Unable to retrieve networks from the Network API: %s", err)
		}

		allPorts, err := ports.ExtractPorts(allPages)
		if err != nil {
			return nil, fmt.Errorf("Unable to retrieve networks from the Network API: %s", err)
		}

		var port ports.Port
		switch len(allPorts) {
		case 0:
			return nil, fmt.Errorf("Could not find any matching port for %s %s", queryType, queryTerm)
		case 1:
			port = allPorts[0]
		default:
			return nil, fmt.Errorf("More than one port found for %s %s", queryType, queryTerm)
		}

		queryType = "id"
		queryTerm = port.NetworkID
	}

	listOpts := networks.ListOpts{
		Status: "ACTIVE",
	}

	switch queryType {
	case "name":
		listOpts.Name = queryTerm
	default:
		listOpts.ID = queryTerm
	}

	allPages, err := networks.List(client, listOpts).AllPages()
	if err != nil {
		return nil, fmt.Errorf("Unable to retrieve networks from the Network API: %s", err)
	}

	allNetworks, err := networks.ExtractNetworks(allPages)
	if err != nil {
		return nil, fmt.Errorf("Unable to retrieve networks from the Network API: %s", err)
	}

	var network networks.Network
	switch len(allNetworks) {
	case 0:
		return nil, fmt.Errorf("Could not find any matching network for %s %s", queryType, queryTerm)
	case 1:
		network = allNetworks[0]
	default:
		return nil, fmt.Errorf("More than one network found for %s %s", queryType, queryTerm)
	}

	v := map[string]interface{}{
		"uuid": network.ID,
		"name": network.Name,
	}

	log.Printf("[DEBUG] getInstanceNetworkInfoNeutron: %#v", v)
	return v, nil
}

// getInstanceAddresses parses a Gophercloud server.Server's Address field into
// a structured InstanceAddresses struct.
func getInstanceAddresses(addresses map[string]interface{}) []InstanceAddresses {
	var allInstanceAddresses []InstanceAddresses

	for networkName, v := range addresses {
		instanceAddresses := InstanceAddresses{
			NetworkName: networkName,
		}

		instanceNIC := InstanceNIC{}
		for _, v := range v.([]interface{}) {
			v := v.(map[string]interface{})
			if v["OS-EXT-IPS:type"] == "fixed" {
				switch v["version"].(float64) {
				case 6:
					instanceNIC.FixedIPv6 = fmt.Sprintf("[%s]", v["addr"].(string))
				default:
					instanceNIC.FixedIPv4 = v["addr"].(string)
				}
			}

			if v, ok := v["OS-EXT-IPS-MAC:mac_addr"].(string); ok {
				instanceNIC.MAC = v
			}
		}
		instanceAddresses.InstanceNICs = append(instanceAddresses.InstanceNICs, instanceNIC)

		allInstanceAddresses = append(allInstanceAddresses, instanceAddresses)
	}

	log.Printf("[DEBUG] Addresses: %#v", addresses)
	log.Printf("[DEBUG] allInstanceAddresses: %#v", allInstanceAddresses)

	return allInstanceAddresses
}

// expandInstanceNetworks takes network information found in []InstanceNetwork
// and builds a Gophercloud []servers.Network for use in creating an Instance.
func expandInstanceNetworks(allInstanceNetworks []InstanceNetwork) []servers.Network {
	var networks []servers.Network
	for _, v := range allInstanceNetworks {
		n := servers.Network{
			UUID:    v.UUID,
			Port:    v.Port,
			FixedIP: v.FixedIP,
		}
		networks = append(networks, n)
	}

	return networks
}

// flattenInstanceNetworks collects instance network information from different
// sources and aggregates it all together into a map array.
func flattenInstanceNetworks(
	d *schema.ResourceData, meta interface{}) ([]map[string]interface{}, error) {

	config := meta.(*Config)
	computeClient, err := config.computeV2Client(GetRegion(d, config))
	if err != nil {
		return nil, fmt.Errorf("Error creating HuaweiCloudStack compute client: %s", err)
	}

	server, err := servers.Get(computeClient, d.Id()).Extract()
	if err != nil {
		return nil, CheckDeleted(d, err, "server")
	}

	allInstanceAddresses := getInstanceAddresses(server.Addresses)
	allInstanceNetworks, err := getAllInstanceNetworks(d, meta)
	if err != nil {
		return nil, err
	}

	networks := []map[string]interface{}{}

	// If there were no instance networks returned, this means that there
	// was not a network specified in the Terraform configuration. When this
	// happens, the instance will be launched on a "default" network, if one
	// is available. If there isn't, the instance will fail to launch, so
	// this is a safe assumption at this point.
	if len(allInstanceNetworks) == 0 {
		for _, instanceAddresses := range allInstanceAddresses {
			for _, instanceNIC := range instanceAddresses.InstanceNICs {
				v := map[string]interface{}{
					"name":        instanceAddresses.NetworkName,
					"fixed_ip_v4": instanceNIC.FixedIPv4,
					"fixed_ip_v6": instanceNIC.FixedIPv6,
					"mac":         instanceNIC.MAC,
				}
				networks = append(networks, v)
			}
		}

		log.Printf("[DEBUG] flattenInstanceNetworks: %#v", networks)
		return networks, nil
	}

	// Loop through all networks and addresses, merge relevant address details.
	for _, instanceNetwork := range allInstanceNetworks {
		for _, instanceAddresses := range allInstanceAddresses {
			if instanceNetwork.Name == instanceAddresses.NetworkName {
				// Only use one NIC since it's possible the user defined another NIC
				// on this same network in another Terraform network block.
				instanceNIC := instanceAddresses.InstanceNICs[0]
				copy(instanceAddresses.InstanceNICs, instanceAddresses.InstanceNICs[1:])
				v := map[string]interface{}{
					"name":           instanceAddresses.NetworkName,
					"fixed_ip_v4":    instanceNIC.FixedIPv4,
					"fixed_ip_v6":    instanceNIC.FixedIPv6,
					"mac":            instanceNIC.MAC,
					"uuid":           instanceNetwork.UUID,
					"port":           instanceNetwork.Port,
					"access_network": instanceNetwork.AccessNetwork,
				}
				networks = append(networks, v)
			}
		}
	}

	log.Printf("[DEBUG] flattenInstanceNetworks: %#v", networks)
	return networks, nil
}

// getInstanceAccessAddresses determines the best IP address to communicate
// with the instance. It does this by looping through all networks and looking
// for a valid IP address. Priority is given to a network that was flagged as
// an access_network.
func getInstanceAccessAddresses(
	d *schema.ResourceData, networks []map[string]interface{}) (string, string) {

	var hostv4, hostv6 string

	// Loop through all networks
	// If the network has a valid fixed v4 or fixed v6 address
	// and hostv4 or hostv6 is not set, set hostv4/hostv6.
	// If the network is an "access_network" overwrite hostv4/hostv6.
	for _, n := range networks {
		var accessNetwork bool

		if an, ok := n["access_network"].(bool); ok && an {
			accessNetwork = true
		}

		if fixedIPv4, ok := n["fixed_ip_v4"].(string); ok && fixedIPv4 != "" {
			if hostv4 == "" || accessNetwork {
				hostv4 = fixedIPv4
			}
		}

		if fixedIPv6, ok := n["fixed_ip_v6"].(string); ok && fixedIPv6 != "" {
			if hostv6 == "" || accessNetwork {
				hostv6 = fixedIPv6
			}
		}
	}

	log.Printf("[DEBUG] HuaweiCloudStack Instance Network Access Addresses: %s, %s", hostv4, hostv6)

	return hostv4, hostv6
}
