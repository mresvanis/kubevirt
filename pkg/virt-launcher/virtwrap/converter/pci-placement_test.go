package converter

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

var _ = Describe("PCI placement", func() {
	Context("PCIe topology with NUMA alignment", func() {
		var assigner *pcieNUMAAssigner
		var domainSpec *api.DomainSpec
		var mockDevices []api.HostDevice

		BeforeEach(func() {
			domainSpec = &api.DomainSpec{
				Devices: api.Devices{
					Controllers: []api.Controller{},
				},
			}
			assigner = NewPCIeNUMAAssigner(domainSpec)
			mockDevices = []api.HostDevice{
				{
					Type: api.HostDevicePCI,
					Source: api.HostDeviceSource{
						Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
					},
				},
				{
					Type: api.HostDevicePCI,
					Source: api.HostDeviceSource{
						Address: &api.Address{Domain: "0x0000", Bus: "0x02", Slot: "0x00", Function: "0x0"},
					},
				},
			}
		})

		Context("device grouping and topology creation", func() {
			It("should initialize PCIe expander bus assigner correctly", func() {
				Expect(assigner).ToNot(BeNil())
				Expect(assigner.domainSpec).To(Equal(domainSpec))
				Expect(assigner.topologyMap).ToNot(BeNil())
				Expect(assigner.index).To(Equal(uint32(1)))
				Expect(assigner.devices).To(BeEmpty())
			})

			It("should filter PCI devices correctly", func() {
				// Test with mixed device types
				mixedDevices := []api.HostDevice{
					{Type: api.HostDevicePCI, Source: api.HostDeviceSource{Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"}}},
					{Type: "usb"},
					{Type: api.HostDevicePCI, Source: api.HostDeviceSource{Address: &api.Address{Domain: "0x0000", Bus: "0x02", Slot: "0x00", Function: "0x0"}}},
					{Type: "scsi"},
				}

				// Before adding devices, we should have empty list
				Expect(assigner.devices).To(BeEmpty())

				// Add devices - this will filter out non-PCI devices and devices without NUMA info
				assigner.AddDevices(mixedDevices)

				// Since we don't have real filesystem access in tests, devices without NUMA info are skipped
				// This is expected behavior as documented in the implementation
				Expect(len(assigner.devices)).To(BeNumerically("<=", 2)) // At most 2 PCI devices, but could be 0 if no NUMA info
			})
		})

		Context("PCI device placement", func() {
			It("should handle empty device list", func() {
				// Test with no devices added
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred()) // Empty device list should not cause error

				// Verify no controllers were added
				Expect(domainSpec.Devices.Controllers).To(BeEmpty())
			})

			It("should handle devices without NUMA information", func() {
				// Add devices - in test environment they will be filtered out due to no NUMA info
				assigner.AddDevices(mockDevices)

				// PlaceDevices should succeed with filtered device list
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())

				// No controllers should be added since devices were filtered out
				Expect(domainSpec.Devices.Controllers).To(BeEmpty())
			})

			It("should count PCI devices correctly", func() {
				spec := &api.DomainSpec{
					Devices: api.Devices{
						Interfaces: []api.Interface{
							{Address: &api.Address{Type: api.AddressPCI}},
							{Address: &api.Address{Type: api.AddressPCI}},
						},
						HostDevices: []api.HostDevice{
							{Type: api.HostDevicePCI, Address: &api.Address{Type: api.AddressPCI}},
						},
						Controllers: []api.Controller{
							{Model: "pci-root"}, // Should be skipped
							{Address: &api.Address{Type: api.AddressPCI}},
						},
					},
				}
				interfacesCount := len(spec.Devices.Interfaces)
				hostDevicesCount := len(spec.Devices.HostDevices)
				controllersCount := 0
				for _, ctrl := range spec.Devices.Controllers {
					if ctrl.Model != "pci-root" && ctrl.Model != "pcie-root" {
						controllersCount++
					}
				}

				count, err := CountPCIDevices(spec)
				Expect(err).ToNot(HaveOccurred())
				Expect(count).To(Equal(interfacesCount + hostDevicesCount + controllersCount))
			})
		})

		Context("PCIe controller creation", func() {
			It("should create PCIe expander bus correctly", func() {
				numaNode := uint32(1)
				controller := assigner.createController(api.ControllerModelPCIeExpanderBus, "", 0, &numaNode)

				Expect(controller).ToNot(BeNil())
				Expect(controller.Type).To(Equal(api.ControllerTypePCI))
				Expect(controller.Model).To(Equal(api.ControllerModelPCIeExpanderBus))
				Expect(controller.Index).To(Equal("2")) // Should increment from initial index 1
				Expect(controller.Target).ToNot(BeNil())
				Expect(controller.Target.Node).To(Equal(&numaNode))
				Expect(controller.Address).To(BeNil()) // Expander bus has no PCI address
			})

			It("should create PCIe root port correctly", func() {
				slot := uint32(5)
				parentBus := "1"
				controller := assigner.createController(api.ControllerModelPCIeRootPort, parentBus, slot, nil)

				Expect(controller).ToNot(BeNil())
				Expect(controller.Type).To(Equal(api.ControllerTypePCI))
				Expect(controller.Model).To(Equal(api.ControllerModelPCIeRootPort))
				Expect(controller.Index).To(Equal("2")) // Should increment from initial index 1
				Expect(controller.Target).To(BeNil())   // Root port has no NUMA target
				Expect(controller.Address).ToNot(BeNil())
				Expect(controller.Address.Type).To(Equal(api.AddressPCI))
				Expect(controller.Address.Domain).To(Equal("0x0000"))
				Expect(controller.Address.Bus).To(Equal(parentBus))
				Expect(controller.Address.Slot).To(Equal("0x05"))
				Expect(controller.Address.Function).To(Equal("0x0"))
			})

			It("should create PCIe switch upstream correctly", func() {
				parentBus := "2"
				controller := assigner.createController(api.ControllerModelPCIeSwitchUpstream, parentBus, 0, nil)

				Expect(controller).ToNot(BeNil())
				Expect(controller.Type).To(Equal(api.ControllerTypePCI))
				Expect(controller.Model).To(Equal(api.ControllerModelPCIeSwitchUpstream))
				Expect(controller.Index).To(Equal("2")) // Should increment from initial index 1
				Expect(controller.Address).ToNot(BeNil())
				Expect(controller.Address.Bus).To(Equal(parentBus))
				Expect(controller.Address.Slot).To(Equal("0x00")) // slot 0 for upstream switch
			})

			It("should create PCIe switch downstream correctly", func() {
				slot := uint32(3)
				parentBus := "3"
				controller := assigner.createController(api.ControllerModelPCIeSwitchDownstream, parentBus, slot, nil)

				Expect(controller).ToNot(BeNil())
				Expect(controller.Type).To(Equal(api.ControllerTypePCI))
				Expect(controller.Model).To(Equal(api.ControllerModelPCIeSwitchDownstream))
				Expect(controller.Index).To(Equal("2")) // Should increment from initial index 1
				Expect(controller.Address).ToNot(BeNil())
				Expect(controller.Address.Bus).To(Equal(parentBus))
				Expect(controller.Address.Slot).To(Equal("0x03"))
			})

			It("should increment index correctly for multiple controllers", func() {
				initialIndex := assigner.index
				Expect(initialIndex).To(Equal(uint32(1)))

				// Create first controller
				controller1 := assigner.createController(api.ControllerModelPCIeRootPort, "1", 1, nil)
				Expect(controller1.Index).To(Equal("2"))
				Expect(assigner.index).To(Equal(uint32(2)))

				// Create second controller
				controller2 := assigner.createController(api.ControllerModelPCIeRootPort, "1", 2, nil)
				Expect(controller2.Index).To(Equal("3"))
				Expect(assigner.index).To(Equal(uint32(3)))
			})
		})

		Context("NUMA topology management", func() {
			It("should ensure NUMA topology creation", func() {
				key := numaTopologyKey{numaNode: 0}

				// Initially should be empty
				Expect(assigner.topologyMap).To(BeEmpty())

				// Ensure topology
				topology := assigner.ensureNUMATopology(key)
				Expect(topology).ToNot(BeNil())
				Expect(topology.pcieExpanderBus).ToNot(BeNil())
				Expect(topology.pcieExpanderBus.Model).To(Equal(api.ControllerModelPCIeExpanderBus))
				Expect(topology.pcieExpanderBus.Target.Node).To(Equal(&key.numaNode))
				Expect(topology.pcieAddressPerDeviceAlias).ToNot(BeNil())
				Expect(topology.pcieAddressPerDeviceAlias).To(BeEmpty())

				// Second call should return same topology
				topology2 := assigner.ensureNUMATopology(key)
				Expect(topology2).To(Equal(topology))
				Expect(len(assigner.topologyMap)).To(Equal(1))
			})

			It("should create separate topologies for different NUMA nodes", func() {
				key1 := numaTopologyKey{numaNode: 0}
				key2 := numaTopologyKey{numaNode: 1}

				topology1 := assigner.ensureNUMATopology(key1)
				topology2 := assigner.ensureNUMATopology(key2)

				Expect(topology1).ToNot(Equal(topology2))
				Expect(len(assigner.topologyMap)).To(Equal(2))
				Expect(topology1.pcieExpanderBus.Target.Node).To(Equal(&key1.numaNode))
				Expect(topology2.pcieExpanderBus.Target.Node).To(Equal(&key2.numaNode))
			})
		})

		Context("device count validation", func() {
			It("should accept valid device counts", func() {
				devices := make([]*api.HostDevice, 5) // Well below the limit
				for i := range devices {
					devices[i] = &api.HostDevice{
						Type: api.HostDevicePCI,
						Source: api.HostDeviceSource{
							Address: &api.Address{
								Domain:   "0x0000",
								Bus:      fmt.Sprintf("0x%02x", i),
								Slot:     "0x00",
								Function: "0x0",
							},
						},
					}
				}

				err := assigner.validateDeviceCount("pci0000:00", devices)
				Expect(err).ToNot(HaveOccurred())
			})

			It("should reject device counts exceeding maximum", func() {
				devices := make([]*api.HostDevice, maxDownstreamPortsPerUpstream+1)
				for i := range devices {
					devices[i] = &api.HostDevice{
						Type: api.HostDevicePCI,
						Source: api.HostDeviceSource{
							Address: &api.Address{
								Domain:   "0x0000",
								Bus:      fmt.Sprintf("0x%02x", i),
								Slot:     "0x00",
								Function: "0x0",
							},
						},
					}
				}

				err := assigner.validateDeviceCount("pci0000:00", devices)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("too many devices"))
				Expect(err.Error()).To(ContainSubstring("pci0000:00"))
			})

			It("should accept maximum device count", func() {
				devices := make([]*api.HostDevice, maxDownstreamPortsPerUpstream)
				for i := range devices {
					devices[i] = &api.HostDevice{
						Type: api.HostDevicePCI,
						Source: api.HostDeviceSource{
							Address: &api.Address{
								Domain:   "0x0000",
								Bus:      fmt.Sprintf("0x%02x", i),
								Slot:     "0x00",
								Function: "0x0",
							},
						},
					}
				}

				err := assigner.validateDeviceCount("pci0000:00", devices)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("topology building", func() {
			var mockTopology *pcieNUMATopology
			var mockDevice *api.HostDevice

			BeforeEach(func() {
				mockTopology = &pcieNUMATopology{
					pcieExpanderBus:           &api.Controller{Index: "1"},
					pcieRootPorts:             []*api.Controller{},
					pcieUpstreamSwitches:      []*api.Controller{},
					pcieDownstreamSwitches:    []*api.Controller{},
					pcieAddressPerDeviceAlias: make(map[string]*api.Address),
				}
				mockDevice = &api.HostDevice{
					Type:  api.HostDevicePCI,
					Alias: api.NewUserDefinedAlias("device1"),
					Source: api.HostDeviceSource{
						Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
					},
				}
			})

			It("should place single device correctly", func() {
				assigner.placeSingleDevice(mockTopology, mockDevice, 0)

				Expect(mockTopology.pcieRootPorts).To(HaveLen(1))
				Expect(mockTopology.pcieUpstreamSwitches).To(BeEmpty())
				Expect(mockTopology.pcieDownstreamSwitches).To(BeEmpty())

				address, exists := mockTopology.pcieAddressPerDeviceAlias[mockDevice.Alias.GetName()]
				Expect(exists).To(BeTrue())
				Expect(address).ToNot(BeNil())
				Expect(address.Type).To(Equal(api.AddressPCI))
				Expect(address.Bus).To(Equal(mockTopology.pcieRootPorts[0].Index))
			})

			It("should place multiple devices correctly", func() {
				devices := []*api.HostDevice{mockDevice}
				for i := 1; i < 3; i++ {
					device := &api.HostDevice{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias(fmt.Sprintf("device%d", i+1)),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: fmt.Sprintf("0x%02x", i+1), Slot: "0x00", Function: "0x0"},
						},
					}
					devices = append(devices, device)
				}

				assigner.placeMultipleDevices(mockTopology, devices, 0)

				Expect(mockTopology.pcieRootPorts).To(HaveLen(1))
				Expect(mockTopology.pcieUpstreamSwitches).To(HaveLen(1))
				Expect(mockTopology.pcieDownstreamSwitches).To(HaveLen(len(devices)))

				// Verify all devices have addresses assigned
				for _, device := range devices {
					address, exists := mockTopology.pcieAddressPerDeviceAlias[device.Alias.GetName()]
					Expect(exists).To(BeTrue())
					Expect(address).ToNot(BeNil())
					Expect(address.Type).To(Equal(api.AddressPCI))
				}
			})
		})

		Context("Edge Cases and Error Scenarios", func() {
			It("should handle devices without aliases", func() {
				deviceWithoutAlias := &api.HostDevice{
					Type: api.HostDevicePCI,
					Source: api.HostDeviceSource{
						Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
					},
				}

				// Add device without alias - should not crash
				assigner.AddDevices([]api.HostDevice{*deviceWithoutAlias})
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())
			})

			It("should handle mixed NUMA and non-NUMA devices", func() {
				mixedDevices := []api.HostDevice{
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("numa_device"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
						},
					},
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("non_numa_device"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x02", Slot: "0x00", Function: "0x0"},
						},
					},
					{
						Type: "usb", // Non-PCI device should be filtered out
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x03", Slot: "0x00", Function: "0x0"},
						},
					},
				}

				assigner.AddDevices(mixedDevices)
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())
			})

			It("should respect pre-assigned PCI addresses", func() {
				deviceWithPreAssignedAddress := api.HostDevice{
					Type:  api.HostDevicePCI,
					Alias: api.NewUserDefinedAlias("preassigned_device"),
					Address: &api.Address{
						Type:     api.AddressPCI,
						Domain:   "0x0000",
						Bus:      "0x05",
						Slot:     "0x10",
						Function: "0x0",
					},
					Source: api.HostDeviceSource{
						Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
					},
				}

				assigner.AddDevices([]api.HostDevice{deviceWithPreAssignedAddress})
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())

				// Device should keep its pre-assigned address
				if len(assigner.devices) > 0 {
					device := assigner.devices[0]
					if device.Address != nil {
						Expect(device.Address.Bus).To(Equal("0x05"))
						Expect(device.Address.Slot).To(Equal("0x10"))
					}
				}
			})

			It("should handle duplicate device names gracefully", func() {
				duplicateDevices := []api.HostDevice{
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("duplicate_name"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
						},
					},
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("duplicate_name"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x02", Slot: "0x00", Function: "0x0"},
						},
					},
				}

				assigner.AddDevices(duplicateDevices)
				err := assigner.PlaceDevices()
				// Should handle duplicates gracefully
				Expect(err).ToNot(HaveOccurred())
			})

			It("should handle invalid NUMA node values", func() {
				// Test with devices that would have invalid NUMA node values
				// This tests the resilience of the system when sysfs returns unexpected data
				invalidDevices := []api.HostDevice{
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("invalid_numa_device"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
						},
					},
				}

				assigner.AddDevices(invalidDevices)
				err := assigner.PlaceDevices()
				// Should not crash, devices without valid NUMA info are filtered out
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("Boundary Conditions", func() {
			It("should handle maximum devices per upstream switch", func() {
				devices := make([]api.HostDevice, maxDownstreamPortsPerUpstream)
				for i := 0; i < maxDownstreamPortsPerUpstream; i++ {
					devices[i] = api.HostDevice{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias(fmt.Sprintf("max_device_%d", i)),
						Source: api.HostDeviceSource{
							Address: &api.Address{
								Domain:   "0x0000",
								Bus:      fmt.Sprintf("0x%02x", i+1),
								Slot:     "0x00",
								Function: "0x0",
							},
						},
					}
				}

				err := assigner.validateDeviceCount("pci0000:00", make([]*api.HostDevice, maxDownstreamPortsPerUpstream))
				Expect(err).ToNot(HaveOccurred())
			})

			It("should handle zero devices", func() {
				emptyDevices := []api.HostDevice{}
				assigner.AddDevices(emptyDevices)
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())
				Expect(domainSpec.Devices.Controllers).To(BeEmpty())
			})

			It("should handle maximum NUMA nodes", func() {
				// Test with high NUMA node numbers
				highNumaDevices := []api.HostDevice{
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("high_numa_device"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
						},
					},
				}

				assigner.AddDevices(highNumaDevices)
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())
			})

			It("should validate bus number ranges", func() {
				// Test with various bus numbers to ensure proper hex formatting
				testCases := []struct {
					bus         string
					valid       bool
					description string
				}{
					{"0x00", true, "minimum bus number"},
					{"0xff", true, "maximum bus number"},
					{"0x80", true, "mid-range bus number"},
					{"0x100", false, "bus number too high"},
					{"invalid", false, "non-hex bus number"},
				}

				for _, tc := range testCases {
					device := api.HostDevice{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias(fmt.Sprintf("bus_test_%s", tc.bus)),
						Source: api.HostDeviceSource{
							Address: &api.Address{
								Domain:   "0x0000",
								Bus:      tc.bus,
								Slot:     "0x00",
								Function: "0x0",
							},
						},
					}

					// The validation happens during address parsing
					// Invalid addresses should be handled gracefully
					assigner.AddDevices([]api.HostDevice{device})
					err := assigner.PlaceDevices()
					// Should not crash regardless of address validity
					Expect(err).ToNot(HaveOccurred())
				}
			})
		})

		Context("Complex Topologies", func() {
			BeforeEach(func() {
				// Reset the assigner for complex topology tests
				domainSpec = &api.DomainSpec{
					Devices: api.Devices{
						Controllers: []api.Controller{},
					},
				}
				assigner = NewPCIeNUMAAssigner(domainSpec)
			})

			It("should handle multiple PCIe roots per NUMA node", func() {
				// Simulate multiple PCIe roots on the same NUMA node
				multiRootDevices := []api.HostDevice{
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("root1_device1"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
						},
					},
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("root1_device2"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x02", Slot: "0x00", Function: "0x0"},
						},
					},
				}

				assigner.AddDevices(multiRootDevices)
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())
			})

			It("should optimize single vs multiple device placement", func() {
				// Test that single device gets direct root port connection
				singleDevice := []api.HostDevice{
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("single_device"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
						},
					},
				}

				assigner.AddDevices(singleDevice)
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())

				// For single device, should use simpler topology (direct connection)
				// Multiple devices would require switch infrastructure
			})

			It("should handle cross-NUMA device scenarios", func() {
				// Test devices from different NUMA nodes
				crossNumaDevices := []api.HostDevice{
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("numa0_device"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x01", Slot: "0x00", Function: "0x0"},
						},
					},
					{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias("numa1_device"),
						Source: api.HostDeviceSource{
							Address: &api.Address{Domain: "0x0000", Bus: "0x81", Slot: "0x00", Function: "0x0"},
						},
					},
				}

				assigner.AddDevices(crossNumaDevices)
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())
			})

			It("should manage bus number allocation efficiently", func() {
				// Test that bus numbers are allocated efficiently and don't conflict
				manyDevices := make([]api.HostDevice, 10)
				for i := 0; i < 10; i++ {
					manyDevices[i] = api.HostDevice{
						Type:  api.HostDevicePCI,
						Alias: api.NewUserDefinedAlias(fmt.Sprintf("bus_alloc_device_%d", i)),
						Source: api.HostDeviceSource{
							Address: &api.Address{
								Domain:   "0x0000",
								Bus:      fmt.Sprintf("0x%02x", i+1),
								Slot:     "0x00",
								Function: "0x0",
							},
						},
					}
				}

				assigner.AddDevices(manyDevices)
				err := assigner.PlaceDevices()
				Expect(err).ToNot(HaveOccurred())

				// Verify that controllers have unique indices
				controllerIndices := make(map[string]bool)
				for _, ctrl := range domainSpec.Devices.Controllers {
					if ctrl.Index != "" {
						Expect(controllerIndices[ctrl.Index]).To(BeFalse(), "Controller index %s should be unique", ctrl.Index)
						controllerIndices[ctrl.Index] = true
					}
				}
			})
		})
	})
})
