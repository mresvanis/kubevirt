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
 * Copyright The KubeVirt Authors.
 *
 */

package hardware

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	v1 "kubevirt.io/api/core/v1"

	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
)

const (
	PCI_ADDRESS_PATTERN = `^([\da-fA-F]{4}):([\da-fA-F]{2}):([\da-fA-F]{2})\.([0-7]{1})$`
)

// Parse linux cpuset into an array of ints
// See: http://man7.org/linux/man-pages/man7/cpuset.7.html#FORMATS
func ParseCPUSetLine(cpusetLine string, limit int) (cpusList []int, err error) {
	elements := strings.Split(cpusetLine, ",")
	for _, item := range elements {
		cpuRange := strings.Split(item, "-")
		// provided a range: 1-3
		if len(cpuRange) > 1 {
			start, err := strconv.Atoi(cpuRange[0])
			if err != nil {
				return nil, err
			}
			end, err := strconv.Atoi(cpuRange[1])
			if err != nil {
				return nil, err
			}
			// Add cpus to the list. Assuming it's a valid range.
			for cpuNum := start; cpuNum <= end; cpuNum++ {
				if cpusList, err = safeAppend(cpusList, cpuNum, limit); err != nil {
					return nil, err
				}
			}
		} else {
			cpuNum, err := strconv.Atoi(cpuRange[0])
			if err != nil {
				return nil, err
			}
			if cpusList, err = safeAppend(cpusList, cpuNum, limit); err != nil {
				return nil, err
			}
		}
	}
	return
}

func safeAppend(cpusList []int, cpu int, limit int) ([]int, error) {
	if len(cpusList) > limit {
		return nil, fmt.Errorf("rejecting expanding CPU array for safety reasons, limit is %v", limit)
	}
	return append(cpusList, cpu), nil
}

// GetNumberOfVCPUs returns number of vCPUs
// It counts sockets*cores*threads
func GetNumberOfVCPUs(cpuSpec *v1.CPU) int64 {
	vCPUs := cpuSpec.Cores
	if cpuSpec.Sockets != 0 {
		if vCPUs == 0 {
			vCPUs = cpuSpec.Sockets
		} else {
			vCPUs *= cpuSpec.Sockets
		}
	}
	if cpuSpec.Threads != 0 {
		if vCPUs == 0 {
			vCPUs = cpuSpec.Threads
		} else {
			vCPUs *= cpuSpec.Threads
		}
	}
	return int64(vCPUs)
}

// ParsePciAddress returns an array of PCI DBSF fields (domain, bus, slot, function)
func ParsePciAddress(pciAddress string) ([]string, error) {
	pciAddrRegx, err := regexp.Compile(PCI_ADDRESS_PATTERN)
	if err != nil {
		return nil, fmt.Errorf("failed to compile pci address pattern, %v", err)
	}
	res := pciAddrRegx.FindStringSubmatch(pciAddress)
	if len(res) == 0 {
		return nil, fmt.Errorf("failed to parse pci address %s", pciAddress)
	}
	return res[1:], nil
}

func GetDeviceNumaNode(pciAddress string) (*uint32, error) {
	pciBasePath := "/sys/bus/pci/devices"
	numaNodePath := filepath.Join(pciBasePath, pciAddress, "numa_node")
	// #nosec No risk for path injection. Reading static path of NUMA node info
	numaNodeStr, err := os.ReadFile(numaNodePath)
	if err != nil {
		return nil, err
	}
	numaNodeStr = bytes.TrimSpace(numaNodeStr)
	numaNodeInt, err := strconv.Atoi(string(numaNodeStr))
	if err != nil {
		return nil, err
	}
	numaNode := uint32(numaNodeInt)
	return &numaNode, nil
}

func GetDeviceAlignedCPUs(pciAddress string) ([]int, error) {
	numaNode, err := GetDeviceNumaNode(pciAddress)
	if err != nil {
		return nil, err
	}
	cpuList, err := GetNumaNodeCPUList(int(*numaNode))
	if err != nil {
		return nil, err
	}
	return cpuList, err
}

func GetNumaNodeCPUList(numaNode int) ([]int, error) {
	filePath := fmt.Sprintf("/sys/bus/node/devices/node%d/cpulist", numaNode)
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	content = bytes.TrimSpace(content)
	cpusList, err := ParseCPUSetLine(string(content[:]), 50000)
	if err != nil {
		return nil, fmt.Errorf("failed to parse cpulist file: %v", err)
	}

	return cpusList, nil
}

func LookupDeviceVCPUAffinity(pciAddress string, domainSpec *api.DomainSpec) ([]uint32, error) {
	alignedVCPUList := []uint32{}
	p2vCPUMap := make(map[string]uint32)
	alignedPhysicalCPUs, err := GetDeviceAlignedCPUs(pciAddress)
	if err != nil {
		return nil, err
	}

	// make sure that the VMI has cpus from this numa node.
	cpuTune := domainSpec.CPUTune.VCPUPin
	for _, vcpuPin := range cpuTune {
		p2vCPUMap[vcpuPin.CPUSet] = vcpuPin.VCPU
	}

	for _, pcpu := range alignedPhysicalCPUs {
		if vCPU, exist := p2vCPUMap[strconv.Itoa(int(pcpu))]; exist {
			alignedVCPUList = append(alignedVCPUList, uint32(vCPU))
		}
	}
	return alignedVCPUList, nil
}

func PCIAddressToString(pciBusID *api.Address) string {
	prefix := "0x"
	return fmt.Sprintf("%s:%s:%s.%s",
		strings.TrimPrefix(pciBusID.Domain, prefix),
		strings.TrimPrefix(pciBusID.Bus, prefix),
		strings.TrimPrefix(pciBusID.Slot, prefix),
		strings.TrimPrefix(pciBusID.Function, prefix))
}

// LookupDeviceVCPUNumaNode looks up the NUMA node of a device based on its PCI address
// and the domain specification of the virtual machine.
//
// It returns a pointer to the NUMA node ID if found, or nil if not found.
func LookupDeviceVCPUNumaNode(pciAddress *api.Address, domainSpec *api.DomainSpec) (numaNode *uint32) {
	if pciAddress == nil || domainSpec == nil ||
		domainSpec.CPU.NUMA == nil {
		return
	}

	// vCPUS by device PCI address
	vCPUList, err := LookupDeviceVCPUAffinity(
		PCIAddressToString(pciAddress),
		domainSpec,
	)
	if err != nil || len(vCPUList) == 0 {
		return
	}

	// guest OS numa node by vCPU
	for i, cell := range domainSpec.CPU.NUMA.Cells {
		vcpusInCell, err := ParseCPUSetLine(cell.CPUs, 5000)
		if err != nil {
			continue
		}

		for _, vcpu := range vcpusInCell {
			if vcpu == int(vCPUList[0]) {
				id, err := strconv.Atoi(domainSpec.CPU.NUMA.Cells[i].ID)
				if err == nil {
					cellID := uint32(id)
					numaNode = &cellID
				}
			}
		}
	}
	return
}

// LookupPCIeRootByPCIBusID retrieves the PCIe Root Complex for a given PCI Bus ID
// in BDF (Bus-Device-Function) format, e.g., "0123:45:1e.7".
//
// It returns a string with the PCIe Root Complex information("pci<domain>:<bus>")
// as a string value or an error if the PCI Bus ID is invalid or the root complex cannot be determined.
//
// ref: https://wiki.xenproject.org/wiki/Bus:Device.Function_(BDF)_Notation
func LookupPCIeRootByPCIBusID(pciBusID *api.Address) (string, error) {
	if pciBusID == nil {
		return "", fmt.Errorf("PCI Bus ID cannot be nil")
	}

	pciAddress := PCIAddressToString(pciBusID)

	bdfRegexp := regexp.MustCompile(`^([0-9a-f]{4}):([0-9a-f]{2}):([0-9a-f]{2})\.([0-9a-f]{1})$`)
	if !bdfRegexp.MatchString(pciAddress) {
		return "", fmt.Errorf("invalid PCI Bus ID format: %s", pciAddress)
	}

	pcieRoot, err := resolvePCIeRoot(pciBusID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve PCIe Root Complex for PCI Bus ID %s: %w", pciAddress, err)
	}

	return pcieRoot, nil
}

// resolvePCIeRoot resolves the PCIe Root for a given PCI Bus ID
// in BDF (Bus-Device-Function) format, e.g., "0123:45:1e.7",
// by inspecting sysfs(/sys/devices).
//
// ref: https://wiki.xenproject.org/wiki/Bus:Device.Function_(BDF)_Notation
//
// /sys/devices has a directory structure which reflects the hardware hierarchy in the system.
// Therefore, the device path may contain intermediate directories (devices).
// Thus, we can not simply find the device path from the PCI Bus ID.
// But, fortunately, /sys/bus/pci/devices/<address> is a symlink to the actual device path in /sys/devices.
// So we can resolve the actual device path by reading the symlink at /sys/bus/pci/devices/<address>.
//
// For example, if the PCIAddress is "0000:00:1f.0",
// /sys/bus/pci/devices/0000:00:1f.0 points to
// /sys/devices/pci0000:01/...<intermediate PCI devices>.../0000:00:1f.0,
// where "pci0000:01" is the PCIe Root.
func resolvePCIeRoot(pciBusID *api.Address) (string, error) {
	pciAddress := PCIAddressToString(pciBusID)

	// e.g. /sys/bus/pci/devices/0000:00:1f.0
	sysBusPath := filepath.Join("/sys/bus/pci/devices", pciAddress)

	target, err := os.Readlink(sysBusPath)
	if err != nil {
		return "", fmt.Errorf("failed to read symlink for PCI Bus ID %s: %w", sysBusPath, err)
	}

	// If the target is a relative path, we need to resolve it relative to the symlink's directory.
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(sysBusPath), target)
	}

	// targetAbs must be /sys/devices/pci0000:01/...<intermediate PCI devices>.../0000:00:1f.0
	devicePathPrefix := "/sys/devices/pci"
	if !strings.HasPrefix(target, devicePathPrefix) {
		return "", fmt.Errorf("symlink target for PCI Bus ID %s is invalid: it must start with %s: %s", pciAddress, devicePathPrefix, target)
	}
	if filepath.Base(target) != pciAddress {
		return "", fmt.Errorf("symlink target for PCI Bus ID %s is invalid: it must end with %s: %s", pciAddress, pciAddress, target)
	}

	// We need to extract the PCIe Root part, which is the first part of the path after /sys/devices/.
	pcieRootPart := strings.Split(strings.TrimPrefix(target, "/sys/devices/"), "/")[0]

	return pcieRootPart, nil
}

// LookupPCIeIndexByPCIBusID looks up device's position within its PCIe root
func LookupPCIeIndexByPCIBusID(pciBusID *api.Address) (*uint32, error) {
	if pciBusID == nil {
		return nil, fmt.Errorf("PCI Bus ID cannot be nil")
	}

	pciAddress := PCIAddressToString(pciBusID)

	pcieRoot, err := LookupPCIeRootByPCIBusID(pciBusID)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup PCIe root for device %s: %w", pciAddress, err)
	}

	// List all devices in the same PCIe root to determine this device's index
	devices, err := listDevicesInPCIeRoot(pcieRoot)
	if err != nil {
		return nil, fmt.Errorf("failed to list devices in PCIe root %s: %w", pcieRoot, err)
	}

	// Find the index of our device in the sorted list
	for i, device := range devices {
		if device == pciAddress {
			index := uint32(i)
			return &index, nil
		}
	}

	return nil, fmt.Errorf("device %s not found in PCIe root %s", pciAddress, pcieRoot)
}

// listDevicesInPCIeRoot lists all PCI devices belonging to a specific PCIe root
func listDevicesInPCIeRoot(pcieRoot string) ([]string, error) {
	devicesPath := "/sys/bus/pci/devices"

	entries, err := os.ReadDir(devicesPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read PCI devices directory: %w", err)
	}

	var devices []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		deviceName := entry.Name()
		symlinkPath := filepath.Join(devicesPath, deviceName)

		target, err := os.Readlink(symlinkPath)
		if err != nil {
			continue // Skip devices we can't read
		}

		// If the target is a relative path, resolve it
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(symlinkPath), target)
		}

		// Check if this device belongs to the same PCIe root
		if strings.Contains(target, pcieRoot) {
			devices = append(devices, deviceName)
		}
	}

	// Sort devices for consistent indexing
	// This ensures the same device always gets the same index
	for i := 0; i < len(devices)-1; i++ {
		for j := i + 1; j < len(devices); j++ {
			if devices[i] > devices[j] {
				devices[i], devices[j] = devices[j], devices[i]
			}
		}
	}

	return devices, nil
}
