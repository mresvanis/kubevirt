/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018-2023 Red Hat, Inc.
 *
 */

package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"libvirt.org/go/libvirtxml"

	vmSchema "kubevirt.io/api/core/v1"
)

const (
	deviceMappingAnnotation = "topology.vm.kubevirt.io/deviceMapping"
)

type DeviceMapping struct {
	PCIAddress string
	NUMANode   int
}

type DeviceGroup struct {
	Devices  []string `json:"devices"`
	ID       string   `json:"id"`
	NUMANode int      `json:"numaNode"`
}

func fixHostdevMode(xmlData []byte) []byte {
	xmlStr := string(xmlData)

	// Find hostdev elements and check if they already have mode attribute
	re := regexp.MustCompile(`<hostdev\s+([^>]*?)>`)

	result := re.ReplaceAllStringFunc(xmlStr, func(match string) string {
		// Check if mode attribute already exists
		if strings.Contains(match, "mode=") {
			return match // Already has mode, don't modify
		}
		// Add mode="subsystem" before the closing >
		return strings.Replace(match, ">", ` mode="subsystem">`, 1)
	})

	return []byte(result)
}

func parsePCIAddress(address string) (domain, bus, slot, function int, err error) {
	// Parse format like "0000:ba:00.0"
	parts := strings.Split(address, ":")
	if len(parts) != 3 {
		return 0, 0, 0, 0, fmt.Errorf("invalid PCI address format: %s", address)
	}

	domainInt64, err := strconv.ParseInt(parts[0], 16, 32)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("invalid domain in PCI address: %s", parts[0])
	}
	domain = int(domainInt64)

	busInt64, err := strconv.ParseInt(parts[1], 16, 32)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("invalid bus in PCI address: %s", parts[1])
	}
	bus = int(busInt64)

	slotFunc := strings.Split(parts[2], ".")
	if len(slotFunc) != 2 {
		return 0, 0, 0, 0, fmt.Errorf("invalid slot.function format: %s", parts[2])
	}

	slotInt64, err := strconv.ParseInt(slotFunc[0], 16, 32)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("invalid slot in PCI address: %s", slotFunc[0])
	}
	slot = int(slotInt64)

	functionInt64, err := strconv.ParseInt(slotFunc[1], 16, 32)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("invalid function in PCI address: %s", slotFunc[1])
	}
	function = int(functionInt64)

	return domain, bus, slot, function, nil
}

type HostdevInfo struct {
	Alias         string
	SourceAddress *libvirtxml.DomainAddressPCI
}

func getExistingHostdevAddresses(domain *libvirtxml.Domain) []string {
	var addresses []string
	for _, hostdev := range domain.Devices.Hostdevs {
		if hostdev.SubsysPCI != nil && hostdev.SubsysPCI.Source != nil && hostdev.SubsysPCI.Source.Address != nil &&
			hostdev.SubsysPCI.Source.Address.Domain != nil && hostdev.SubsysPCI.Source.Address.Bus != nil &&
			hostdev.SubsysPCI.Source.Address.Slot != nil && hostdev.SubsysPCI.Source.Address.Function != nil {
			hostdevAddr := fmt.Sprintf("%04x:%02x:%02x.%x",
				*hostdev.SubsysPCI.Source.Address.Domain,
				*hostdev.SubsysPCI.Source.Address.Bus,
				*hostdev.SubsysPCI.Source.Address.Slot,
				*hostdev.SubsysPCI.Source.Address.Function)
			addresses = append(addresses, hostdevAddr)
		}
	}
	return addresses
}

func removeExistingHostdevs(domain *libvirtxml.Domain, deviceAddresses []string) map[string]HostdevInfo {
	addressSet := make(map[string]bool)
	for _, addr := range deviceAddresses {
		addressSet[addr] = true
	}

	hostdevInfoMap := make(map[string]HostdevInfo)
	var newHostdevs []libvirtxml.DomainHostdev
	for _, hostdev := range domain.Devices.Hostdevs {
		if hostdev.SubsysPCI != nil && hostdev.SubsysPCI.Source != nil && hostdev.SubsysPCI.Source.Address != nil &&
			hostdev.SubsysPCI.Source.Address.Domain != nil && hostdev.SubsysPCI.Source.Address.Bus != nil &&
			hostdev.SubsysPCI.Source.Address.Slot != nil && hostdev.SubsysPCI.Source.Address.Function != nil {
			hostdevAddr := fmt.Sprintf("%04x:%02x:%02x.%x",
				*hostdev.SubsysPCI.Source.Address.Domain,
				*hostdev.SubsysPCI.Source.Address.Bus,
				*hostdev.SubsysPCI.Source.Address.Slot,
				*hostdev.SubsysPCI.Source.Address.Function)

			if addressSet[hostdevAddr] {
				info := HostdevInfo{
					SourceAddress: hostdev.SubsysPCI.Source.Address,
				}
				if hostdev.Alias != nil && hostdev.Alias.Name != "" {
					info.Alias = hostdev.Alias.Name
				}
				hostdevInfoMap[hostdevAddr] = info
				// Print the XML of the hostdev being removed
				if removedXML, err := xml.MarshalIndent(hostdev, "", "  "); err == nil {
					fmt.Fprintf(os.Stderr, "topology: Removing hostdev XML:\n%s\n", string(removedXML))
				}
			} else {
				newHostdevs = append(newHostdevs, hostdev)
			}
		} else {
			newHostdevs = append(newHostdevs, hostdev)
		}
	}

	domain.Devices.Hostdevs = newHostdevs
	return hostdevInfoMap
}

func findMaxPCIControllerIndex(domain *libvirtxml.Domain) int {
	maxIndex := 1
	for _, controller := range domain.Devices.Controllers {
		if controller.PCI != nil && controller.Index != nil {
			fmt.Fprintf(os.Stderr, "topology: Found existing PCI controller: %+v\n", controller)
			if int(*controller.Index) > maxIndex {
				maxIndex = int(*controller.Index)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "topology: Current max PCI controller index: %d\n", maxIndex)
	return maxIndex
}

// findOccupiedSlots returns a set of occupied PCI slots on bus 0
func findOccupiedSlots(domain *libvirtxml.Domain) map[uint]bool {
	occupied := make(map[uint]bool)

	// Check all controllers
	for _, controller := range domain.Devices.Controllers {
		if controller.Address != nil && controller.Address.PCI != nil {
			addr := controller.Address.PCI
			if addr.Bus != nil && *addr.Bus == 0 && addr.Slot != nil {
				occupied[*addr.Slot] = true
			}
		}
	}

	// Check all other devices that might have PCI addresses
	for _, hostdev := range domain.Devices.Hostdevs {
		if hostdev.Address != nil && hostdev.Address.PCI != nil {
			addr := hostdev.Address.PCI
			if addr.Bus != nil && *addr.Bus == 0 && addr.Slot != nil {
				occupied[*addr.Slot] = true
			}
		}
	}

	return occupied
}

// findAvailableSlot finds the next available PCI slot starting from startSlot
func findAvailableSlot(domain *libvirtxml.Domain, startSlot uint) uint {
	occupied := findOccupiedSlots(domain)

	for slot := startSlot; slot <= 0x1E; slot++ {
		// Skip known reserved slots
		if slot == 0x1B || slot == 0x1F {
			continue
		}
		if !occupied[slot] {
			return slot
		}
	}

	// Fallback to any available slot in safe range
	for slot := uint(0x02); slot <= 0x1E; slot++ {
		if slot == 0x1B || slot == 0x1F {
			continue
		}
		if !occupied[slot] {
			return slot
		}
	}

	// Last resort - use slot 0x02 and hope for the best
	return 0x02
}

func createPCIExpanderBus(domain *libvirtxml.Domain, index int, busNr int, numaNode int) libvirtxml.DomainController {
	indexUint := uint(index)
	busNrUint := uint(busNr)
	numaNodeUint := uint(numaNode)
	domainUint := uint(0)
	busUint := uint(0)
	// Dynamically find an available slot to avoid conflicts
	// Start looking from higher slots and work our way down if needed
	startSlot := uint(0x18) // Start from slot 0x18
	slotUint := findAvailableSlot(domain, startSlot)
	functionUint := uint(0) // Always use function 0

	return libvirtxml.DomainController{
		Type:  "pci",
		Index: &indexUint,
		Model: "pcie-expander-bus",
		PCI: &libvirtxml.DomainControllerPCI{
			Target: &libvirtxml.DomainControllerPCITarget{
				BusNr:    &busNrUint,
				NUMANode: &numaNodeUint,
			},
		},
		Address: &libvirtxml.DomainAddress{
			PCI: &libvirtxml.DomainAddressPCI{
				Domain:   &domainUint,
				Bus:      &busUint,
				Slot:     &slotUint,
				Function: &functionUint,
			},
		},
	}
}

func createPCIRootPort(index int, bus int, slot int) libvirtxml.DomainController {
	indexUint := uint(index)
	domainUint := uint(0)
	busUint := uint(bus)
	slotUint := uint(slot)
	functionUint := uint(0)
	multifunction := "on"
	chassisUint := uint(index)
	portUint := uint(slot)

	return libvirtxml.DomainController{
		Type:  "pci",
		Index: &indexUint,
		Model: "pcie-root-port",
		PCI: &libvirtxml.DomainControllerPCI{
			Target: &libvirtxml.DomainControllerPCITarget{
				Chassis: &chassisUint,
				Port:    &portUint,
			},
		},
		Address: &libvirtxml.DomainAddress{
			PCI: &libvirtxml.DomainAddressPCI{
				Domain:        &domainUint,
				Bus:           &busUint,
				Slot:          &slotUint,
				Function:      &functionUint,
				MultiFunction: multifunction,
			},
		},
	}
}

func createPCISwitchUpstreamPort(index int, bus int) libvirtxml.DomainController {
	indexUint := uint(index)
	domainUint := uint(0)
	busUint := uint(bus)
	slotUint := uint(0)
	functionUint := uint(0)

	return libvirtxml.DomainController{
		Type:  "pci",
		Index: &indexUint,
		Model: "pcie-switch-upstream-port",
		Address: &libvirtxml.DomainAddress{
			PCI: &libvirtxml.DomainAddressPCI{
				Domain:   &domainUint,
				Bus:      &busUint,
				Slot:     &slotUint,
				Function: &functionUint,
			},
		},
	}
}

func createPCISwitchDownstreamPort(index int, bus int) libvirtxml.DomainController {
	indexUint := uint(index)
	domainUint := uint(0)
	busUint := uint(bus)
	slotUint := uint(0)
	functionUint := uint(0)

	return libvirtxml.DomainController{
		Type:  "pci",
		Index: &indexUint,
		Model: "pcie-switch-downstream-port",
		Address: &libvirtxml.DomainAddress{
			PCI: &libvirtxml.DomainAddressPCI{
				Domain:   &domainUint,
				Bus:      &busUint,
				Slot:     &slotUint,
				Function: &functionUint,
			},
		},
	}
}

func createHostdev(pciAddr string, bus int, guestFunction int, alias string, originalAlias string) (libvirtxml.DomainHostdev, error) {
	domain, busAddr, slot, function, err := parsePCIAddress(pciAddr)
	if err != nil {
		return libvirtxml.DomainHostdev{}, err
	}

	domainUint := uint(domain)
	busAddrUint := uint(busAddr)
	slotUint := uint(slot)
	functionUint := uint(function)

	guestDomainUint := uint(0)
	guestBusUint := uint(bus)
	guestSlotUint := uint(0)                 // PCIe devices should use slot 0
	guestFunctionUint := uint(guestFunction) // Use different functions instead

	managed := "no"

	// Use original alias if provided, otherwise use the generated alias
	finalAlias := alias
	if originalAlias != "" {
		finalAlias = originalAlias
	}

	return libvirtxml.DomainHostdev{
		Managed: managed,
		SubsysPCI: &libvirtxml.DomainHostdevSubsysPCI{
			Source: &libvirtxml.DomainHostdevSubsysPCISource{
				Address: &libvirtxml.DomainAddressPCI{
					Domain:   &domainUint,
					Bus:      &busAddrUint,
					Slot:     &slotUint,
					Function: &functionUint,
				},
			},
		},
		Alias: &libvirtxml.DomainAlias{
			Name: finalAlias,
		},
		Address: &libvirtxml.DomainAddress{
			PCI: &libvirtxml.DomainAddressPCI{
				Domain:   &guestDomainUint,
				Bus:      &guestBusUint,
				Slot:     &guestSlotUint,
				Function: &guestFunctionUint,
			},
		},
	}, nil
}

func updatePCITopology(domain *libvirtxml.Domain, deviceGroups []DeviceGroup) error {
	// If no device groups to process, nothing to do
	if len(deviceGroups) == 0 {
		fmt.Fprintf(os.Stderr, "topology: No device groups to process, preserving existing hostdevs\n")
		return nil
	}

	// Find current max PCI controller index and start from the next available index
	maxIndex := findMaxPCIControllerIndex(domain)
	currentIndex := max(maxIndex+1, 1)

	// Only remove hostdevs that belong to device groups and capture their info
	var groupedDeviceAddresses []string
	for _, group := range deviceGroups {
		groupedDeviceAddresses = append(groupedDeviceAddresses, group.Devices...)
	}

	fmt.Fprintf(os.Stderr, "topology: Removing %d grouped hostdevs for topology update: %v\n", len(groupedDeviceAddresses), groupedDeviceAddresses)
	originalHostdevInfo := removeExistingHostdevs(domain, groupedDeviceAddresses)

	// Group devices by NUMA node to create expander buses
	numaNodes := make(map[int]bool)
	for _, group := range deviceGroups {
		numaNodes[group.NUMANode] = true
	}

	// Create expander buses for each NUMA node
	expandedBusMap := make(map[int]int)   // numaNode -> bus index
	numaSlotCounters := make(map[int]int) // numaNode -> next available slot
	for numaNode := range numaNodes {
		// Calculate bus number safely, ensuring positive values and avoiding conflicts
		// Use a base of 0x10 and increment by 20 for each NUMA node starting from 0
		busNr := max(0x10+numaNode*20, 0x10)
		expanderBus := createPCIExpanderBus(domain, currentIndex, busNr, numaNode)
		// Print the XML of the expander bus being added
		if busXML, err := xml.MarshalIndent(expanderBus, "", "  "); err == nil {
			fmt.Fprintf(os.Stderr, "topology: Adding PCI expander bus XML:\n%s\n", string(busXML))
		}
		domain.Devices.Controllers = append(domain.Devices.Controllers, expanderBus)
		expandedBusMap[numaNode] = currentIndex
		numaSlotCounters[numaNode] = 0 // Start slot assignment from 0
		currentIndex++
	}

	// Create PCI topology for each device group
	deviceCounter := 0
	for _, group := range deviceGroups {
		expanderBusIndex := expandedBusMap[group.NUMANode]
		currentSlot := numaSlotCounters[group.NUMANode]

		// Root port for this group
		rootPortIndex := currentIndex
		rootPort := createPCIRootPort(rootPortIndex, expanderBusIndex, currentSlot)
		// Print the XML of the root port being added
		if rootPortXML, err := xml.MarshalIndent(rootPort, "", "  "); err == nil {
			fmt.Fprintf(os.Stderr, "topology: Adding PCI root port XML for group %s:\n%s\n", group.ID, string(rootPortXML))
		}
		domain.Devices.Controllers = append(domain.Devices.Controllers, rootPort)
		numaSlotCounters[group.NUMANode]++
		currentIndex++

		// Switch upstream port for this group
		upstreamIndex := currentIndex
		upstream := createPCISwitchUpstreamPort(upstreamIndex, rootPortIndex)
		// Print the XML of the upstream port being added
		if upstreamXML, err := xml.MarshalIndent(upstream, "", "  "); err == nil {
			fmt.Fprintf(os.Stderr, "topology: Adding PCI switch upstream port XML for group %s:\n%s\n", group.ID, string(upstreamXML))
		}
		domain.Devices.Controllers = append(domain.Devices.Controllers, upstream)
		currentIndex++

		// Create hostdevs for all devices in this group - all connected to their own downstream switch port
		for _, deviceAddr := range group.Devices {
			downstream := createPCISwitchDownstreamPort(currentIndex, upstreamIndex)
			// Print the XML of the downstream port being added
			if downstreamXML, err := xml.MarshalIndent(downstream, "", "  "); err == nil {
				fmt.Fprintf(os.Stderr, "topology: Adding PCI switch downstream port XML for group %s:\n%s\n", group.ID, string(downstreamXML))
			}
			domain.Devices.Controllers = append(domain.Devices.Controllers, downstream)

			alias := fmt.Sprintf("hostdev%d", deviceCounter)
			originalInfo := originalHostdevInfo[deviceAddr]
			hostdev, err := createHostdev(deviceAddr, currentIndex, 0, alias, originalInfo.Alias)
			if err != nil {
				return fmt.Errorf("failed to create hostdev for %s in group %s: %v", deviceAddr, group.ID, err)
			}
			currentIndex++

			// Print the XML of the hostdev being added
			if hostdevXML, err := xml.MarshalIndent(hostdev, "", "  "); err == nil {
				fmt.Fprintf(os.Stderr, "topology: Adding hostdev XML for device %s in group %s (function %d):\n%s\n", deviceAddr, group.ID, 0, string(hostdevXML))
			}
			domain.Devices.Hostdevs = append(domain.Devices.Hostdevs, hostdev)
			deviceCounter++
		}
	}

	return nil
}

func parseDeviceGroups(annotation string) ([]DeviceGroup, error) {
	var deviceGroups []DeviceGroup
	if err := json.Unmarshal([]byte(annotation), &deviceGroups); err != nil {
		return nil, fmt.Errorf("failed to unmarshal device groups JSON: %v", err)
	}
	return deviceGroups, nil
}

func getAvailableNUMANodes(domain *libvirtxml.Domain) []int {
	var numaNodes []int

	if domain.CPU != nil && domain.CPU.Numa != nil {
		for _, cell := range domain.CPU.Numa.Cell {
			if cell.ID != nil {
				numaNodes = append(numaNodes, int(*cell.ID))
			}
		}
	}

	return numaNodes
}

func filterDeviceGroupsByNUMATopology(deviceGroups []DeviceGroup, availableNUMANodes []int) []DeviceGroup {
	// Create a set of available NUMA nodes for quick lookup
	numaNodeSet := make(map[int]bool)
	for _, node := range availableNUMANodes {
		numaNodeSet[node] = true
	}

	var filteredGroups []DeviceGroup
	for _, group := range deviceGroups {
		// Only include groups that reference existing NUMA nodes
		if numaNodeSet[group.NUMANode] {
			filteredGroups = append(filteredGroups, group)
		} else {
			fmt.Fprintf(os.Stderr, "topology: Filtering out device group %s: NUMA node %d not found in domain topology (available nodes: %v)\n",
				group.ID, group.NUMANode, availableNUMANodes)
		}
	}

	return filteredGroups
}

func filterDeviceGroupsByExistingHostdevs(deviceGroups []DeviceGroup, existingAddresses []string) []DeviceGroup {
	// Create a set of existing addresses for quick lookup
	existingSet := make(map[string]bool)
	for _, addr := range existingAddresses {
		existingSet[addr] = true
	}

	var filteredGroups []DeviceGroup
	for _, group := range deviceGroups {
		var devicesInGroup []string
		// Only include devices that exist in the domain
		for _, device := range group.Devices {
			if existingSet[device] {
				devicesInGroup = append(devicesInGroup, device)
			}
		}

		// Only include the group if it has devices that exist in the domain
		if len(devicesInGroup) > 0 {
			filteredGroup := group
			filteredGroup.Devices = devicesInGroup
			filteredGroups = append(filteredGroups, filteredGroup)
		}
	}

	return filteredGroups
}

func onDefineDomain(vmiJSON, domainXML []byte) (string, error) {
	vmiSpec := vmSchema.VirtualMachineInstance{}
	if err := json.Unmarshal(vmiJSON, &vmiSpec); err != nil {
		return "", fmt.Errorf("failed to unmarshal given VMI spec: %s %s", err, string(vmiJSON))
	}

	fixedXML := fixHostdevMode(domainXML)
	domainSpec := libvirtxml.Domain{}
	if err := xml.Unmarshal(fixedXML, &domainSpec); err != nil {
		return "", fmt.Errorf("failed to unmarshal given Domain spec: %s %s", err, string(domainXML))
	}

	// Check if domain.Devices.Controllers contain any PCI controller
	hasPCIController := false
	for _, controller := range domainSpec.Devices.Controllers {
		if controller.PCI != nil {
			hasPCIController = true
			break
		}
	}
	if !hasPCIController {
		// No PCI controllers, nothing to domainXML modify
		fmt.Fprintf(os.Stderr, "topology: No PCI controllers found, skipping topology modification\n")
		return string(domainXML), nil
	}

	annotations := vmiSpec.GetAnnotations()
	dm, found := annotations[deviceMappingAnnotation]

	var filteredDeviceGroups []DeviceGroup
	if found {
		// Get existing hostdev addresses from the domain
		existingAddresses := getExistingHostdevAddresses(&domainSpec)

		// Parse device groups from annotation
		deviceGroups, err := parseDeviceGroups(dm)
		if err != nil {
			return "", fmt.Errorf("failed to parse device groups annotation: %s", err)
		}

		// Filter device groups to only include devices that exist in the domain
		filteredDeviceGroups = filterDeviceGroupsByExistingHostdevs(deviceGroups, existingAddresses)

		// Log the filtering results
		fmt.Fprintf(os.Stderr, "topology: Found %d device groups in annotation, %d groups have devices in domain\n", len(deviceGroups), len(filteredDeviceGroups))
		for _, group := range filteredDeviceGroups {
			fmt.Fprintf(os.Stderr, "topology: Group %s has %d devices in domain: %v\n", group.ID, len(group.Devices), group.Devices)
		}
	}

	// Check if NUMA nodes in the domain match those in the device groups
	// If not, filter out device groups that don't align with the NUMA topology
	if len(filteredDeviceGroups) > 0 {
		availableNUMANodes := getAvailableNUMANodes(&domainSpec)
		fmt.Fprintf(os.Stderr, "topology: Domain has NUMA nodes: %v\n", availableNUMANodes)

		if len(availableNUMANodes) > 0 {
			// Filter device groups to only include those with valid NUMA nodes
			filteredDeviceGroups = filterDeviceGroupsByNUMATopology(filteredDeviceGroups, availableNUMANodes)
			fmt.Fprintf(os.Stderr, "topology: After NUMA filtering: %d device groups remain\n", len(filteredDeviceGroups))
		} else {
			fmt.Fprintf(os.Stderr, "topology: No NUMA topology found in domain, skipping NUMA validation\n")
		}
	}

	err := updatePCITopology(&domainSpec, filteredDeviceGroups)
	if err != nil {
		return "", fmt.Errorf("failed to update PCI topology: %s", err)
	}

	newDomainXML, err := xml.MarshalIndent(domainSpec, "", "\t")
	if err != nil {
		return "", fmt.Errorf("failed to marshal new Domain spec: %s %+v", err, domainSpec)
	}

	return string(newDomainXML), nil
}

func main() {
	var vmiJSON, domainXML string
	pflag.StringVar(&vmiJSON, "vmi", "", "VMI to change in JSON format")
	pflag.StringVar(&domainXML, "domain", "", "Domain spec in XML format")
	pflag.Parse()

	logger := log.New(os.Stderr, "topology", log.Ldate)
	if vmiJSON == "" || domainXML == "" {
		logger.Printf("Bad input vmi=%d, domain=%d", len(vmiJSON), len(domainXML))
		os.Exit(1)
	}

	domainXML, err := onDefineDomain([]byte(vmiJSON), []byte(domainXML))
	if err != nil {
		logger.Printf("onDefineDomain failed: %s", err)
		panic(err)
	}
	fmt.Println(domainXML)
}
