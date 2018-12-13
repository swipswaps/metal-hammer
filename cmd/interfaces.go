package cmd

import (
	"fmt"
	"git.f-i-ts.de/cloud-native/metal/metal-hammer/metal-core/models"
	"git.f-i-ts.de/cloud-native/metal/metal-hammer/pkg/lldp"
	"git.f-i-ts.de/cloud-native/metallib/version"
	log "github.com/inconshreveable/log15"
	"github.com/jaypipes/ghw"
	"github.com/vishvananda/netlink"
	"strings"
	"time"
)

// UpAllInterfaces set all available eth* interfaces up
// to ensure they do ipv6 link local autoconfiguration and
// therefore neighbor discovery,
// which is required to make all local mac's visible on the switch side.
func (h *Hammer) UpAllInterfaces() error {
	net, err := ghw.Network()
	if err != nil {
		return fmt.Errorf("Error getting network info: %v", err)
	}

	description := fmt.Sprintf("metal-hammer IP:%s version:%s waiting since %s for installation", h.IPAddress, version.V, h.Started)
	interfaces := make([]string, 0)
	for _, nic := range net.NICs {
		if !strings.HasPrefix(nic.Name, "eth") {
			continue
		}
		interfaces = append(interfaces, nic.Name)

		err := linkSetUp(nic.Name)
		if err != nil {
			return fmt.Errorf("Error set link %s up: %v", nic.Name, err)
		}

		lldpd, err := lldp.NewDaemon(h.Spec.DeviceUUID, description, nic.Name, 5*time.Second)

		if err != nil {
			return fmt.Errorf("Error start lldpd on %s info: %v", nic.Name, err)
		}
		lldpd.Start()
	}

	lc := NewLLDPClient(interfaces)
	h.LLDPClient = lc
	go lc.Start()

	return nil
}

func linkSetUp(name string) error {
	iface, err := netlink.LinkByName(name)
	if err != nil {
		return err
	}
	err = netlink.LinkSetUp(iface)
	if err != nil {
		return err
	}
	return nil
}

// Neighbors of a interface, detected via ip neighbor detection
func (h *Hammer) Neighbors(name string) ([]*models.ModelsMetalNic, error) {
	neighbors := make([]*models.ModelsMetalNic, 0)

	host := h.LLDPClient.Host

	for !host.done {
		log.Info("not all lldp pdu's received, waiting...", "interface", name)
		time.Sleep(1 * time.Second)

		duration := time.Now().Sub(host.start)
		if duration > LLDPTxIntervalTimeout {
			return nil, fmt.Errorf("not all neighbor requirements where met within: %s, exiting", LLDPTxIntervalTimeout)
		}
	}
	log.Info("all lldp pdu's received", "interface", name)

	neighs, _ := host.neighbors[name]
	for _, neigh := range neighs {
		if neigh.Port.Type != lldp.Mac {
			continue
		}
		macAddress := neigh.Port.Value
		neighbors = append(neighbors, &models.ModelsMetalNic{Mac: &macAddress})
	}
	return neighbors, nil
}
