package converter

import (
	"fmt"

	"k8s.io/utils/ptr"
	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/client-go/log"

	"kubevirt.io/kubevirt/pkg/util/hardware"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

const (
	maxExpanderBusNr = 254

	maxDownstreamPortsPerUpstream = 32
)

// iteratePCIAddresses invokes the callback function for each PCI device specified in the domain
func iteratePCIAddresses(spec *api.DomainSpec, callback func(address *api.Address) (*api.Address, error)) (err error) {
	fn := func(address *api.Address) (*api.Address, error) {
		if address == nil || address.Type == "" || address.Type == api.AddressPCI {
			return callback(address)
		}
		return address, nil
	}
	for i, iface := range spec.Devices.Interfaces {
		spec.Devices.Interfaces[i].Address, err = fn(iface.Address)
		if err != nil {
			return err
		}
	}
	for i, hostDev := range spec.Devices.HostDevices {
		if hostDev.Type != api.HostDevicePCI {
			continue
		}
		spec.Devices.HostDevices[i].Address, err = fn(hostDev.Address)
		if err != nil {
			return err
		}
	}
	for i, controller := range spec.Devices.Controllers {
		// pci-root and pcie-root devices can by definition hot have a pci address on its own
		if controller.Model == "pci-root" || controller.Model == "pcie-root" || controller.Model == api.ControllerModelPCIeExpanderBus {
			continue
		}
		spec.Devices.Controllers[i].Address, err = fn(controller.Address)
		if err != nil {
			return err
		}
	}
	for i, disk := range spec.Devices.Disks {
		if disk.Target.Bus != v1.DiskBusVirtio {
			continue
		}
		spec.Devices.Disks[i].Address, err = fn(disk.Address)
		if err != nil {
			return err
		}
	}
	for i, input := range spec.Devices.Inputs {
		if input.Bus != v1.VirtIO {
			continue
		}
		spec.Devices.Inputs[i].Address, err = fn(input.Address)
		if err != nil {
			return err
		}
	}
	for i, watchdog := range spec.Devices.Watchdogs {
		spec.Devices.Watchdogs[i].Address, err = fn(watchdog.Address)
		if err != nil {
			return err
		}
	}
	if spec.Devices.Rng != nil {
		spec.Devices.Rng.Address, err = fn(spec.Devices.Rng.Address)
		if err != nil {
			return err
		}
	}
	if spec.Devices.Ballooning != nil {
		spec.Devices.Ballooning.Address, err = fn(spec.Devices.Ballooning.Address)
		if err != nil {
			return err
		}
	}
	return nil
}

func CountPCIDevices(spec *api.DomainSpec) (count int, err error) {
	err = iteratePCIAddresses(spec, func(address *api.Address) (*api.Address, error) {
		count++
		return address, nil
	})
	return count, err
}

func PlacePCIDevicesOnRootComplex(spec *api.DomainSpec) (err error) {
	assigner := newRootSlotAssigner()
	return iteratePCIAddresses(spec, assigner.PlacePCIDeviceAtNextSlot)
}

func (p *pciRootSlotAssigner) nextSlot() (int, error) {
	slot := p.slot + 1
	// reserved slots are:
	// slot 0
	// slot 1 for VGA
	// slot 0x1f for a sata controller from  qemu
	// slot 0x1b for the first ich9 sound card
	switch slot {
	case 0, 0x01:
		slot = 0x02
	case 0x1f, 0x1b:
		slot = slot + 1
	}

	if slot >= 0x20 {
		return slot, fmt.Errorf("No space left on the root PCI bus.")
	}
	p.slot = slot
	return slot, nil
}

func newRootSlotAssigner() *pciRootSlotAssigner {
	return &pciRootSlotAssigner{slot: -1}
}

type pciRootSlotAssigner struct {
	slot int
}

// newPCIAddress creates a PCI address with the specified bus and slot.
func newPCIAddress(bus string, slot string) *api.Address {
	return &api.Address{
		Type:     api.AddressPCI,
		Domain:   "0x0000",
		Bus:      bus,
		Slot:     slot,
		Function: "0x0",
	}
}

func (p *pciRootSlotAssigner) PlacePCIDeviceAtNextSlot(address *api.Address) (*api.Address, error) {
	if address == nil {
		address = &api.Address{}
	}

	// keep explicit requests for pci addresses
	if address.Domain != "" {
		return address, nil
	}

	slot, err := p.nextSlot()
	if err != nil {
		return nil, err
	}
	address.Type = api.AddressPCI
	address.Domain = "0x0000"
	address.Bus = "0x00"
	address.Slot = fmt.Sprintf("%#02x", slot)
	address.Function = "0x0"
	return address, nil
}

// numaTopologyKey represents a unique NUMA node for expander bus placement.
type numaTopologyKey struct {
	numaNode uint32
}

// pcieNUMATopology represents the PCIe topology for a specific NUMA node.
type pcieNUMATopology struct {
	pcieExpanderBus           *api.Controller
	pcieRootPorts             []*api.Controller
	pcieUpstreamSwitches      []*api.Controller
	pcieDownstreamSwitches    []*api.Controller
	pcieAddressPerDeviceAlias map[string]*api.Address
}

// pcieNUMAAssigner manages the assignment of PCIe expander buses and
// NUMA aligned device placement.
type pcieNUMAAssigner struct {
	domainSpec  *api.DomainSpec
	index       uint32
	topologyMap map[numaTopologyKey]*pcieNUMATopology
	devices     []*api.HostDevice
}

// NewPCIeNUMAAssigner creates a new PCIe expander bus assigner.
func NewPCIeNUMAAssigner(domainSpec *api.DomainSpec) *pcieNUMAAssigner {
	assigner := &pcieNUMAAssigner{
		domainSpec:  domainSpec,
		topologyMap: make(map[numaTopologyKey]*pcieNUMATopology),
		devices:     []*api.HostDevice{},
		index:       1,
	}

	return assigner
}

func (a *pcieNUMAAssigner) createController(model string, parentBus string, slot uint32, numaNode *uint32) *api.Controller {
	a.index++

	controller := &api.Controller{
		Type:  api.ControllerTypePCI,
		Index: fmt.Sprint(a.index),
		Model: model,
	}

	// PCIe expander bus doesn't have a PCI address and has a NUMA target
	if model == api.ControllerModelPCIeExpanderBus {
		controller.Target = &api.ControllerTarget{
			Node: numaNode,
		}
		return controller
	}

	// All other controllers have PCI addresses
	slotStr := "0x00"
	if slot > 0 {
		slotStr = fmt.Sprintf("%#02x", slot)
	}

	controller.Address = newPCIAddress(parentBus, slotStr)

	return controller
}

// AddDevices queues host devices for NUMA aligned placement.
// Call PlaceDevices() after adding all devices.
func (a *pcieNUMAAssigner) AddDevices(devices []api.HostDevice) {
	for i := range devices {
		if devices[i].Type != api.HostDevicePCI {
			continue
		}
		guestOSNumaNode := hardware.LookupDeviceVCPUNumaNode(
			devices[i].Source.Address,
			a.domainSpec,
		)

		if guestOSNumaNode == nil {
			log.Log.Infof("device %s has no NUMA node information, skipping for pcie-expander-bus assignment",
				hardware.PCIAddressToString(devices[i].Source.Address))
			continue
		}
		a.devices = append(a.devices, &devices[i])
	}
}

// pcieDeviceGroups represents a mapping of PCIe root to a list of devices.
type pcieDeviceGroups map[string][]*api.HostDevice

// numaDeviceGroups represents a mapping of NUMA node to pcieDeviceGroups.
type numaDeviceGroups map[numaTopologyKey]pcieDeviceGroups

// numaDeviceGroups groups devices by their NUMA node and PCIe root.
func (a *pcieNUMAAssigner) groupDevicesByNUMA() (numaDeviceGroups, error) {
	groups := make(numaDeviceGroups)

	for _, device := range a.devices {
		pciAddress := hardware.PCIAddressToString(device.Source.Address)

		numaNode := hardware.LookupDeviceVCPUNumaNode(device.Source.Address, a.domainSpec)
		if numaNode == nil {
			return nil, fmt.Errorf("skipping device %s as it has no NUMA node information", pciAddress)
		}

		pcieRoot, err := hardware.LookupPCIeRootByPCIBusID(device.Source.Address)
		if err != nil {
			return nil, fmt.Errorf("failed to lookup PCIe root for device %s: %w", pciAddress, err)
		}

		key := numaTopologyKey{
			numaNode: *numaNode,
		}

		if groups[key] == nil {
			groups[key] = make(pcieDeviceGroups)
		}
		groups[key][pcieRoot] = append(groups[key][pcieRoot], device)
	}

	return groups, nil
}

// ensureNUMATopology handles topology creation/retrieval from map.
// Creates expander bus if topology doesn't exist and returns the topology for the NUMA node.
func (a *pcieNUMAAssigner) ensureNUMATopology(numaKey numaTopologyKey) *pcieNUMATopology {
	topology, exists := a.topologyMap[numaKey]
	if !exists {
		topology = &pcieNUMATopology{
			pcieExpanderBus:           a.createController(api.ControllerModelPCIeExpanderBus, "", 0, &numaKey.numaNode),
			pcieAddressPerDeviceAlias: make(map[string]*api.Address),
		}
		a.topologyMap[numaKey] = topology
	}
	return topology
}

// validateDeviceCount checks if the number of devices exceeds the maximum supported
// by a PCIe upstream switch.
func (a *pcieNUMAAssigner) validateDeviceCount(pcieRoot string, devices []*api.HostDevice) error {
	if len(devices) > maxDownstreamPortsPerUpstream {
		return fmt.Errorf(
			"too many devices found on pcie-root %s, pcie-switch-upstream-port can have up to %d downstream ports",
			pcieRoot,
			maxDownstreamPortsPerUpstream)
	}
	return nil
}

// addRootPort creates a PCIe root port and adds it to the topology.
func (a *pcieNUMAAssigner) addRootPort(topology *pcieNUMATopology, parentBus string, slot uint32) *api.Controller {
	rootPort := a.createController(api.ControllerModelPCIeRootPort, parentBus, slot, nil)
	topology.pcieRootPorts = append(topology.pcieRootPorts, rootPort)
	return rootPort
}

// addUpstreamSwitch creates a PCIe upstream switch and adds it to the topology.
func (a *pcieNUMAAssigner) addUpstreamSwitch(topology *pcieNUMATopology, parentBus string) *api.Controller {
	upstreamSwitch := a.createController(api.ControllerModelPCIeSwitchUpstream, parentBus, 0, nil)
	topology.pcieUpstreamSwitches = append(topology.pcieUpstreamSwitches, upstreamSwitch)
	return upstreamSwitch
}

// addDownstreamSwitch creates a PCIe downstream switch and adds it to the topology.
func (a *pcieNUMAAssigner) addDownstreamSwitch(topology *pcieNUMATopology, parentBus string, slot uint32) *api.Controller {
	downstreamSwitch := a.createController(api.ControllerModelPCIeSwitchDownstream, parentBus, slot, nil)
	topology.pcieDownstreamSwitches = append(topology.pcieDownstreamSwitches, downstreamSwitch)
	return downstreamSwitch
}

// placeSingleDevice handles the simple case where only one device from a PCIe root
// is placed. Creates root port and assigns device address directly to it.
func (a *pcieNUMAAssigner) placeSingleDevice(topology *pcieNUMATopology, device *api.HostDevice, rootPortSlot uint32) {
	rootPort := a.addRootPort(topology, topology.pcieExpanderBus.Index, rootPortSlot)
	topology.pcieAddressPerDeviceAlias[device.Alias.GetName()] = newPCIAddress(rootPort.Index, "0x00")
}

// placeMultipleDevices handles the complex case with upstream/downstream switches.
// Creates root port, upstream switch, and downstream switches.
// Assigns device addresses to downstream switch buses.
func (a *pcieNUMAAssigner) placeMultipleDevices(topology *pcieNUMATopology, devices []*api.HostDevice, rootPortSlot uint32) {
	rootPort := a.addRootPort(topology, topology.pcieExpanderBus.Index, rootPortSlot)
	upstreamSwitch := a.addUpstreamSwitch(topology, rootPort.Index)

	for _, device := range devices {
		slot := uint32(len(topology.pcieDownstreamSwitches))
		downstreamSwitch := a.addDownstreamSwitch(topology, upstreamSwitch.Index, slot)
		topology.pcieAddressPerDeviceAlias[device.Alias.GetName()] = newPCIAddress(downstreamSwitch.Index, "0x00")
	}
}

// placeDevicesForPCIeRoot places devices from a PCIe root, choosing between single
// or multiple device placement logic based on the number of devices.
func (a *pcieNUMAAssigner) placeDevicesForPCIeRoot(topology *pcieNUMATopology, devices []*api.HostDevice) {
	rootPortSlot := uint32(len(topology.pcieRootPorts))

	if len(devices) == 1 {
		a.placeSingleDevice(topology, devices[0], rootPortSlot)
	} else {
		a.placeMultipleDevices(topology, devices, rootPortSlot)
	}
}

// buildHierarchy groups devices by NUMA node by using a pcie-expander-bus per
// NUMA node. Within a pcie-expander-bus, devices are grouped by their pcie-root
// and placed on consecutive pcie-switch-downstream-ports under a
// pcie-switch-upstream-port, which in turn is under a pcie-root-port.
//
// pcie-expander-bus (one per NUMA node) -> pcie-root-port (one per PCIe root) -> pcie-switch-upstream-port -> pcie-switch-downstream-ports -> devices
//
// It modifies the topologyMap in place by creating the necessary controllers
// and updating the addresses of the devices.
func (a *pcieNUMAAssigner) buildHierarchy() error {
	numaDeviceGroups, err := a.groupDevicesByNUMA()
	if err != nil {
		return fmt.Errorf("failed to generate device groups per NUMA node and PCIe root: %w", err)
	}

	for numaKey, pcieRootGroups := range numaDeviceGroups {
		topology := a.ensureNUMATopology(numaKey)

		for pcieRoot, devices := range pcieRootGroups {
			if err := a.validateDeviceCount(pcieRoot, devices); err != nil {
				return err
			}

			a.placeDevicesForPCIeRoot(topology, devices)
		}

		// Set the busNr of the expander bus so that it has enough space for all its children.
		// We start from 254 and go downwards to leave space for system controllers and other expander buses.
		busNr := maxExpanderBusNr - a.index
		topology.pcieExpanderBus.Target.BusNr = ptr.To(uint32(busNr))
	}

	return nil
}

// PlaceDevices places the added devices into a PCIe topology aligned to their
// NUMA node. It modifies the domainSpec in place or leaves it unchanged in case
// of an error.
func (a *pcieNUMAAssigner) PlaceDevices() error {
	if err := a.buildHierarchy(); err != nil {
		return fmt.Errorf("failed to create PCIe topology with NUMA alignment: %w", err)
	}

	for _, topology := range a.topologyMap {
		a.domainSpec.Devices.Controllers = append(a.domainSpec.Devices.Controllers, *topology.pcieExpanderBus)

		for _, rootPort := range topology.pcieRootPorts {
			a.domainSpec.Devices.Controllers = append(a.domainSpec.Devices.Controllers, *rootPort)
		}
		for _, upstreamSwitch := range topology.pcieUpstreamSwitches {
			a.domainSpec.Devices.Controllers = append(a.domainSpec.Devices.Controllers, *upstreamSwitch)
		}
		for _, downstreamSwitch := range topology.pcieDownstreamSwitches {
			a.domainSpec.Devices.Controllers = append(a.domainSpec.Devices.Controllers, *downstreamSwitch)
		}
		for _, device := range a.devices {
			if address, exists := topology.pcieAddressPerDeviceAlias[device.Alias.GetName()]; exists {
				device.Address = address
			}
			// If the device was not placed in the topology (e.g. missing CPU
			// affinity information), we leave it unmodified so that it can be
			// placed by the root slot assigner.
		}
	}

	return nil
}
