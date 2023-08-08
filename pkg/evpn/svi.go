// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2022-2023 Intel Corporation, or its subsidiaries.
// Copyright (c) 2022-2023 Dell Inc, or its subsidiaries.

// Package evpn is the main package of the application
package evpn

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"path"

	"github.com/vishvananda/netlink"

	pb "github.com/opiproject/opi-api/network/evpn-gw/v1alpha1/gen/go"

	"go.einride.tech/aip/fieldbehavior"
	"go.einride.tech/aip/fieldmask"
	"go.einride.tech/aip/resourceid"
	"go.einride.tech/aip/resourcename"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// CreateSvi executes the creation of the VLAN
func (s *Server) CreateSvi(_ context.Context, in *pb.CreateSviRequest) (*pb.Svi, error) {
	log.Printf("CreateSvi: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// see https://google.aip.dev/133#user-specified-ids
	resourceID := resourceid.NewSystemGenerated()
	if in.SviId != "" {
		err := resourceid.ValidateUserSettable(in.SviId)
		if err != nil {
			log.Printf("error: %v", err)
			return nil, err
		}
		log.Printf("client provided the ID of a resource %v, ignoring the name field %v", in.SviId, in.Svi.Name)
		resourceID = in.SviId
	}
	in.Svi.Name = resourceIDToFullName("svis", resourceID)
	// idempotent API when called with same key, should return same object
	obj, ok := s.Svis[in.Svi.Name]
	if ok {
		log.Printf("Already existing Svi with id %v", in.Svi.Name)
		return obj, nil
	}
	// not found, so create a new one
	bridge, err := netlink.LinkByName(tenantbridgeName)
	if err != nil {
		err := status.Errorf(codes.NotFound, "unable to find key %s", tenantbridgeName)
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a LogicalBridge resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Svi.Spec.LogicalBridge); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// now get LogicalBridge object to fetch VID field
	bridgeObject, ok := s.Bridges[in.Svi.Spec.LogicalBridge]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Svi.Spec.LogicalBridge)
		log.Printf("error: %v", err)
		return nil, err
	}
	vid := uint16(bridgeObject.Spec.VlanId)
	// Example: bridge vlan add dev br-tenant vid <vlan-id> self
	if err := netlink.BridgeVlanAdd(bridge, vid, false, false, true, false); err != nil {
		fmt.Printf("Failed to add vlan to bridge: %v", err)
		return nil, err
	}
	// Example: ip link add link br-tenant name <link_svi> type vlan id <vlan-id>
	vlandev := &netlink.Vlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        resourceID,
			ParentIndex: bridge.Attrs().Index,
		},
		VlanId: int(bridgeObject.Spec.VlanId),
	}
	// Example: ip link set <link_svi> addr aa:bb:cc:00:00:41
	if len(in.Svi.Spec.MacAddress) > 0 {
		if err := netlink.LinkSetHardwareAddr(vlandev, in.Svi.Spec.MacAddress); err != nil {
			fmt.Printf("Failed to set MAC on link: %v", err)
			return nil, err
		}
	}
	// Example: ip address add <svi-ip-with prefixlength> dev <link_svi>
	for _, gwip := range in.Svi.Spec.GwIpPrefix {
		fmt.Printf("Assign the GW IP address %v to the SVI interface %v", gwip, vlandev)
		myip := make(net.IP, 4)
		binary.BigEndian.PutUint32(myip, gwip.Addr.GetV4Addr())
		addr := &netlink.Addr{IPNet: &net.IPNet{IP: myip, Mask: net.CIDRMask(int(gwip.Len), 32)}}
		if err := netlink.AddrAdd(vlandev, addr); err != nil {
			fmt.Printf("Failed to set IP on link: %v", err)
			return nil, err
		}
	}
	// Validate that a Vrf resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Svi.Spec.Vrf); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// now get Vrf to plug this vlandev into
	vrf, ok := s.Vrfs[in.Svi.Spec.Vrf]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Svi.Spec.Vrf)
		log.Printf("error: %v", err)
		return nil, err
	}
	// get net device by name
	vrfdev, err := netlink.LinkByName(path.Base(vrf.Name))
	if err != nil {
		err := status.Errorf(codes.NotFound, "unable to find key %s", vrf.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	// Example: ip link set <link_svi> master <vrf-name> up
	if err := netlink.LinkSetMaster(vlandev, vrfdev); err != nil {
		fmt.Printf("Failed to add vlandev to vrf: %v", err)
		return nil, err
	}
	// Example: ip link set <link_svi> up
	if err := netlink.LinkSetUp(vlandev); err != nil {
		fmt.Printf("Failed to up link: %v", err)
		return nil, err
	}
	response := proto.Clone(in.Svi).(*pb.Svi)
	response.Status = &pb.SviStatus{OperStatus: pb.SVIOperStatus_SVI_OPER_STATUS_UP}
	s.Svis[in.Svi.Name] = response
	log.Printf("CreateSvi: Sending to client: %v", response)
	return response, nil
}

// DeleteSvi deletes a VLAN
func (s *Server) DeleteSvi(_ context.Context, in *pb.DeleteSviRequest) (*emptypb.Empty, error) {
	log.Printf("DeleteSvi: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Name); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	obj, ok := s.Svis[in.Name]
	if !ok {
		if in.AllowMissing {
			return &emptypb.Empty{}, nil
		}
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	resourceID := path.Base(obj.Name)
	// use netlink to find vlan
	vlan, err := netlink.LinkByName(resourceID)
	if err != nil {
		err := status.Errorf(codes.NotFound, "unable to find key %s", resourceID)
		log.Printf("error: %v", err)
		return nil, err
	}
	// bring link down
	if err := netlink.LinkSetDown(vlan); err != nil {
		fmt.Printf("Failed to up link: %v", err)
		return nil, err
	}
	// use netlink to delete vlan
	if err := netlink.LinkDel(vlan); err != nil {
		fmt.Printf("Failed to delete link: %v", err)
		return nil, err
	}
	// remove from the Database
	delete(s.Svis, obj.Name)
	return &emptypb.Empty{}, nil
}

// UpdateSvi updates an VLAN
func (s *Server) UpdateSvi(_ context.Context, in *pb.UpdateSviRequest) (*pb.Svi, error) {
	log.Printf("UpdateSvi: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Svi.Name); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	svi, ok := s.Svis[in.Svi.Name]
	if !ok {
		// TODO: introduce "in.AllowMissing" field. In case "true", create a new resource, don't return error
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Svi.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	// update_mask = 2
	if err := fieldmask.Validate(in.UpdateMask, in.Svi); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	resourceID := path.Base(svi.Name)
	iface, err := netlink.LinkByName(resourceID)
	if err != nil {
		err := status.Errorf(codes.NotFound, "unable to find key %s", resourceID)
		log.Printf("error: %v", err)
		return nil, err
	}
	// base := iface.Attrs()
	// iface.MTU = 1500 // TODO: remove this, just an example
	if err := netlink.LinkModify(iface); err != nil {
		fmt.Printf("Failed to update link: %v", err)
		return nil, err
	}
	response := proto.Clone(in.Svi).(*pb.Svi)
	response.Status = &pb.SviStatus{OperStatus: pb.SVIOperStatus_SVI_OPER_STATUS_UP}
	s.Svis[in.Svi.Name] = response
	log.Printf("UpdateSvi: Sending to client: %v", response)
	return response, nil
}

// GetSvi gets an VLAN
func (s *Server) GetSvi(_ context.Context, in *pb.GetSviRequest) (*pb.Svi, error) {
	log.Printf("GetSvi: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Name); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	obj, ok := s.Svis[in.Name]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	resourceID := path.Base(obj.Name)
	_, err := netlink.LinkByName(resourceID)
	if err != nil {
		err := status.Errorf(codes.NotFound, "unable to find key %s", resourceID)
		log.Printf("error: %v", err)
		return nil, err
	}
	// TODO
	return &pb.Svi{Name: in.Name, Spec: &pb.SviSpec{MacAddress: obj.Spec.MacAddress, EnableBgp: obj.Spec.EnableBgp, RemoteAs: obj.Spec.RemoteAs}, Status: &pb.SviStatus{OperStatus: pb.SVIOperStatus_SVI_OPER_STATUS_UP}}, nil
}