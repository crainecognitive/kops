/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudup

import (
	"fmt"
	"net"
	"sort"

	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/util/subnet"
	"k8s.io/kops/upup/pkg/fi"
)

// ByZone implements sort.Interface for []*ClusterSubnetSpec based on
// the Zone field.
type ByZone []*kops.ClusterSubnetSpec

func (a ByZone) Len() int {
	return len(a)
}

func (a ByZone) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a ByZone) Less(i, j int) bool {
	return a[i].Zone < a[j].Zone
}

func assignCIDRsToSubnets(c *kops.Cluster, cloud fi.Cloud) error {
	// TODO: We probably could query for the existing subnets & allocate appropriately
	// for now we'll require users to set CIDRs themselves

	if allSubnetsHaveCIDRs(c) {
		klog.V(4).Infof("All subnets have CIDRs; skipping assignment logic")
		return nil
	}

	if c.Spec.Networking.NetworkID != "" {

		vpcInfo, err := cloud.FindVPCInfo(c.Spec.Networking.NetworkID)
		if err != nil {
			return err
		}
		if vpcInfo == nil {
			return fmt.Errorf("VPC %q not found", c.Spec.Networking.NetworkID)
		}

		subnetByID := make(map[string]*fi.SubnetInfo)
		for _, subnetInfo := range vpcInfo.Subnets {
			subnetByID[subnetInfo.ID] = subnetInfo
		}
		for i := range c.Spec.Networking.Subnets {
			subnet := &c.Spec.Networking.Subnets[i]
			if subnet.ID != "" {
				cloudSubnet := subnetByID[subnet.ID]
				if cloudSubnet == nil {
					return fmt.Errorf("Subnet %q not found in VPC %q", subnet.ID, c.Spec.Networking.NetworkID)
				}
				if subnet.CIDR == "" {
					subnet.CIDR = cloudSubnet.CIDR
					if subnet.CIDR == "" {
						return fmt.Errorf("Subnet %q did not have CIDR", subnet.ID)
					}
				} else if subnet.CIDR != cloudSubnet.CIDR {
					return fmt.Errorf("Subnet %q has configured CIDR %q, but the actual CIDR found was %q", subnet.ID, subnet.CIDR, cloudSubnet.CIDR)
				}

				if subnet.Zone != cloudSubnet.Zone {
					return fmt.Errorf("Subnet %q has configured Zone %q, but the actual Zone found was %q", subnet.ID, subnet.Zone, cloudSubnet.Zone)
				}

			}
		}
	}

	if allSubnetsHaveCIDRs(c) {
		klog.V(4).Infof("All subnets have CIDRs; skipping assignment logic")
		return nil
	}

	_, cidr, err := net.ParseCIDR(c.Spec.Networking.NetworkCIDR)
	if err != nil {
		return fmt.Errorf("Invalid NetworkCIDR: %q", c.Spec.Networking.NetworkCIDR)
	}

	// We split the network range into 2, 4 or 8 subnets
	// But we then reserve the lowest one for the private block
	// (and we split _that_ into 8 further subnets, leaving the first one unused/for future use)

	var bigSubnets []*kops.ClusterSubnetSpec
	var littleSubnets []*kops.ClusterSubnetSpec

	var reserved []*net.IPNet
	for i := range c.Spec.Networking.Subnets {
		subnet := &c.Spec.Networking.Subnets[i]
		if subnet.CIDR != "" {
			_, cidrSubnet, err := net.ParseCIDR(subnet.CIDR)
			if err != nil {
				return fmt.Errorf("invalid subnet %q CIDR: %q", subnet.Name, subnet.CIDR)
			}
			// Skip additional subnets
			if !cidr.Contains(cidrSubnet.IP) {
				continue
			}
		}
		switch subnet.Type {
		case kops.SubnetTypeDualStack, kops.SubnetTypePublic, kops.SubnetTypePrivate:
			bigSubnets = append(bigSubnets, subnet)

		case kops.SubnetTypeUtility:
			littleSubnets = append(littleSubnets, subnet)

		default:
			return fmt.Errorf("subnet %q has unknown type %q", subnet.Name, subnet.Type)
		}

		if subnet.CIDR != "" {
			_, subnetCIDR, err := net.ParseCIDR(subnet.CIDR)
			if err != nil {
				return fmt.Errorf("subnet %q has unexpected CIDR %q", subnet.Name, subnet.CIDR)
			}

			reserved = append(reserved, subnetCIDR)
		}
	}

	// Assign a consistent order
	sort.Sort(ByZone(bigSubnets))
	sort.Sort(ByZone(littleSubnets))

	// Check how many subnet slices are needed
	cidrCount := len(bigSubnets)
	if len(littleSubnets) > 0 {
		cidrCount += 1
	}
	var bigCIDRs []*net.IPNet
	if cidrCount <= 1 {
		bigCIDRs, err = subnet.SplitInto1(cidr)
	} else if cidrCount <= 2 {
		bigCIDRs, err = subnet.SplitInto2(cidr)
	} else if cidrCount <= 4 {
		bigCIDRs, err = subnet.SplitInto4(cidr)
	} else {
		bigCIDRs, err = subnet.SplitInto8(cidr)
	}
	if err != nil {
		return err
	}

	// Remove any CIDRs marked as overlapping
	{
		var nonOverlapping []*net.IPNet
		for _, c := range bigCIDRs {
			overlapped := false
			for _, r := range reserved {
				if subnet.Overlap(r, c) {
					overlapped = true
				}
			}
			if !overlapped {
				nonOverlapping = append(nonOverlapping, c)
			}
		}
		bigCIDRs = nonOverlapping
	}

	if len(bigCIDRs) == 0 {
		return fmt.Errorf("could not find any non-overlapping CIDRs in parent NetworkCIDR; cannot automatically assign CIDR to subnet")
	}

	// Assign CIDRs to little subnets
	if len(littleSubnets) > 0 {
		littleCIDRs, err := subnet.SplitInto8(bigCIDRs[0])
		if err != nil {
			return err
		}
		bigCIDRs = bigCIDRs[1:]

		for _, subnet := range littleSubnets {
			if subnet.CIDR != "" {
				continue
			}

			if len(littleCIDRs) == 0 {
				return fmt.Errorf("insufficient (little) CIDRs remaining for automatic CIDR allocation to subnet %q", subnet.Name)
			}
			subnet.CIDR = littleCIDRs[0].String()
			klog.Infof("Assigned CIDR %s to subnet %s", subnet.CIDR, subnet.Name)

			littleCIDRs = littleCIDRs[1:]
		}
	}

	// Assign CIDRs to big subnets
	for _, subnet := range bigSubnets {
		if subnet.CIDR != "" {
			continue
		}
		if subnet.IPv6CIDR != "" && subnet.Type == kops.SubnetTypePrivate {
			continue
		}

		if len(bigCIDRs) == 0 {
			return fmt.Errorf("insufficient (big) CIDRs remaining for automatic CIDR allocation to subnet %q", subnet.Name)
		}
		subnet.CIDR = bigCIDRs[0].String()
		klog.Infof("Assigned CIDR %s to subnet %s", subnet.CIDR, subnet.Name)

		bigCIDRs = bigCIDRs[1:]
	}

	return nil
}

// allSubnetsHaveCIDRs returns true iff each subnet in the cluster has a non-empty CIDR
func allSubnetsHaveCIDRs(c *kops.Cluster) bool {
	for i := range c.Spec.Networking.Subnets {
		subnet := &c.Spec.Networking.Subnets[i]
		if subnet.CIDR == "" {
			return false
		}
	}

	return true
}
