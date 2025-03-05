package ipam

import (
	"context"
	"net"

	whereaboutsallocate "github.com/k8snetworkplumbingwg/whereabouts/pkg/allocate"
	whereaboutslogging "github.com/k8snetworkplumbingwg/whereabouts/pkg/logging"
	whereaboutstypes "github.com/k8snetworkplumbingwg/whereabouts/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"k8s.io/apimachinery/pkg/types"
)

type ipamAction = func(
	ipRange RangeConfiguration,
	reservation []whereaboutstypes.IPReservation,
) (net.IPNet, []whereaboutstypes.IPReservation, error)

// makeAcquireAction creates a callback which changes IPPool state to include a new IP reservation.
func makeAcquireAction(ctx context.Context, vmName types.NamespacedName) ipamAction {
	return func(ipRange RangeConfiguration, reservation []whereaboutstypes.IPReservation) (net.IPNet, []whereaboutstypes.IPReservation, error) {
		return doAcquire(ctx, ipRange, reservation, vmName)
	}
}

// makeReleaseAction creates a callback which changes IPPool state to deallocate an IP reservation.
func makeReleaseAction(ctx context.Context, vmName types.NamespacedName) ipamAction {
	return func(ipRange RangeConfiguration, reservation []whereaboutstypes.IPReservation) (net.IPNet, []whereaboutstypes.IPReservation, error) {
		return doRelease(ctx, ipRange, reservation, vmName)
	}
}

func doAcquire(
	_ context.Context,
	ipRange RangeConfiguration,
	reservation []whereaboutstypes.IPReservation,
	vmName types.NamespacedName,
) (net.IPNet, []whereaboutstypes.IPReservation, error) {
	// reduce whereabouts logging
	whereaboutslogging.SetLogLevel("error")

	_, ipnet, _ := net.ParseCIDR(ipRange.Range)

	// check if IP reserved for VM already
	foundidx := getMatchingIPReservationIndex(reservation, vmName.String())
	if foundidx >= 0 {
		return net.IPNet{IP: reservation[foundidx].IP, Mask: ipnet.Mask}, reservation, nil
	}

	// try to reserve new IP gor given VM
	ip, newReservation, err := whereaboutsallocate.IterateForAssignment(*ipnet,
		ipRange.RangeStart, ipRange.RangeEnd,
		reservation, ipRange.OmitRanges, vmName.String(), "")
	if err != nil {
		return net.IPNet{}, nil, err
	}

	return net.IPNet{IP: ip, Mask: ipnet.Mask}, newReservation, nil
}

func doRelease(
	ctx context.Context,
	ipRange RangeConfiguration,
	reservation []whereaboutstypes.IPReservation,
	vmName types.NamespacedName,
) (net.IPNet, []whereaboutstypes.IPReservation, error) {
	// reduce whereabouts logging
	whereaboutslogging.SetLogLevel("error")

	log := log.FromContext(ctx)

	_, ipnet, _ := net.ParseCIDR(ipRange.Range)

	// try to release IP for given VM
	newReservation, ip, err := whereaboutsallocate.IterateForDeallocation(reservation, vmName.String(), getMatchingIPReservationIndex)
	if err != nil {
		// The only reason to get an error here is if we are trying
		// to deallocate the same IP twice.
		log.Info("Failed to deallocate IP", "error", err)

		// Ignore the error.
		return net.IPNet{IP: ip, Mask: ipnet.Mask}, newReservation, nil
	}

	return net.IPNet{IP: ip, Mask: ipnet.Mask}, newReservation, nil
}

func getMatchingIPReservationIndex(reservation []whereaboutstypes.IPReservation, id string) int {
	foundidx := -1
	for idx, v := range reservation {
		if v.ContainerID == id {
			foundidx = idx
			break
		}
	}
	return foundidx
}
