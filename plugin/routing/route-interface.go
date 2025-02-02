package routing

import (
	log "github.com/Sirupsen/logrus"
	"github.com/gopher-net/ipvlan-docker-plugin/plugin/routing/gobgp"
	"net"
)

var routemanager RoutingInterface

type RoutingInterface interface {
	StartMonitoring() error
	AdvertizeNewRoute(localPrefix *net.IPNet) error
	WithdrawRoute(localPrefix *net.IPNet) error
}

func InitRoutingMonitering(masterIface string, managermode string, serveraddr net.IP, as string) {
	switch managermode {
	case "gobgp":
		log.Infof("Routing manager is %s", managermode)
		routemanager = gobgp.NewBgpRouteManager(masterIface, serveraddr, as)
	default:
		log.Infof("Default Routing manager: Gobgp")
		routemanager = gobgp.NewBgpRouteManager(masterIface, serveraddr, as)
	}
	error := routemanager.StartMonitoring()
	if error != nil {
		log.Fatal(error)
	}
}
func withdrawRoute(localPrefix *net.IPNet) error {
	error := routemanager.WithdrawRoute(localPrefix)
	if error != nil {
		return error
	}
	return nil
}
func AdvertizeNewRoute(localPrefix *net.IPNet) error {
	error := routemanager.AdvertizeNewRoute(localPrefix)
	if error != nil {
		return error
	}
	return nil
}
