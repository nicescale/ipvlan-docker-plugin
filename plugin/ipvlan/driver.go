package ipvlan

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/codegangsta/cli"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/iptables"
	"github.com/docker/libnetwork/types"
	"github.com/gopher-net/ipvlan-docker-plugin/plugin/routing"
	"github.com/gorilla/mux"
	"github.com/samalba/dockerclient"
	"github.com/vishvananda/netlink"
)

const (
	MethodReceiver     = "NetworkDriver"
	defaultRoute       = "0.0.0.0/0"
	containerEthPrefix = "eth"
	ipVlanL2           = "l2"
	ipVlanL3           = "l3"
	ipVlanL3Routing    = "l3routing"
	minMTU             = 68
	defaultMTU         = 1500
	TxQueueLen         = 0
)

type ipvlanType netlink.IPVlanMode

const (
	_ ipvlanType = iota
	IPVLAN_MODE_L2
	IPVLAN_MODE_L3
)

type Driver interface {
	Listen(string) error
}

type driver struct {
	dockerer
	networkID  string
	gateway    string
	cidr       *net.IPNet
	nameserver string
	pluginConfig
	networks networkTable
	sync.Mutex
}

// Struct for binding plugin specific configurations (cli.go for details).
type pluginConfig struct {
	mtu             int
	mode            string
	hostIface       string
	containerSubnet *net.IPNet
	gatewayIP       net.IP
}

type pluginNet struct {
	netid     string
	hostiface string
}

type endpoint struct {
	id      string
	mac     net.HardwareAddr
	addr    *net.IPNet
	srcName string
}

type endpointTable map[string]*endpoint

func New(version string, ctx *cli.Context) (Driver, error) {
	docker, err := dockerclient.NewDockerClient("unix:///var/run/docker.sock", nil)
	if err != nil {
		return nil, fmt.Errorf("could not connect to docker: %s", err)
	}
	ipVlanEthIface = ctx.String("host-interface")
	// bind CLI opts to the user config struct
	if ok := validateHostIface(ctx.String("host-interface")); !ok {
		log.Debugf("Field [ host-interface ] not detected. Assuming it will be passed via docker network -o (opts)")
	}
	// lower bound of v4 MTU is 68-bytes per rfc791
	if ctx.Int("mtu") <= 0 {
		cliMTU = defaultMTU
	} else if ctx.Int("mtu") >= minMTU {
		cliMTU = ctx.Int("mtu")
	} else {
		log.Fatalf("The MTU value passed [ %d ] must be greater then [ %d ] bytes per rfc791", ctx.Int("mtu"), minMTU)
	}

	switch ctx.String("mode") {
	case ipVlanL2:
		ipVlanMode = ipVlanL2
	case ipVlanL3:
		// IPVlan simply needs the container interface for its
		// default route target since only unicast is allowed <3
		ipVlanMode = ipVlanL3
	case ipVlanL3Routing:
		// IPVlan simply needs the container interface for its
		// default route target since only unicast is allowed <3
		ipVlanMode = ipVlanL3Routing
		//containerGW = nil
		managermode := ""
		serveraddr := net.ParseIP("127.0.0.1")
		as := "65000"
		if ctx.String("routemng") != "" {
			managermode = ctx.String("routemng")
		}
		if ctx.String("serveraddr") != "" {
			serveraddr = net.ParseIP(ctx.String("serveraddr"))
		}
		if ctx.String("as") != "" {
			as = ctx.String("as")
		}
		// Initialize Routing monitoring
		go routing.InitRoutingMonitering(ipVlanEthIface, managermode, serveraddr, as)

	default:
		log.Debugf("Field [ mode ] not detected. Assuming it will be passed via docker network -o (opts)")
	}

	pluginOpts := &pluginConfig{
		mtu:       cliMTU,
		mode:      ipVlanMode,
		hostIface: ipVlanEthIface,
	}

	d := &driver{
		networks: networkTable{},
		dockerer: dockerer{
			client: docker,
		},
		pluginConfig: *pluginOpts,
	}
	return d, nil
}

func (driver *driver) Listen(socket string) error {
	router := mux.NewRouter()
	router.NotFoundHandler = http.HandlerFunc(notFound)

	router.Methods("POST").Path("/Plugin.Activate").HandlerFunc(driver.handshake)
	router.Methods("POST").Path("/NetworkDriver.GetCapabilities").HandlerFunc(driver.capabilities)

	handleMethod := func(method string, h http.HandlerFunc) {
		router.Methods("POST").Path(fmt.Sprintf("/%s.%s", MethodReceiver, method)).HandlerFunc(h)
	}
	handleMethod("CreateNetwork", driver.createNetwork)
	handleMethod("DeleteNetwork", driver.deleteNetwork)
	handleMethod("CreateEndpoint", driver.createEndpoint)
	handleMethod("DeleteEndpoint", driver.deleteEndpoint)
	handleMethod("EndpointOperInfo", driver.infoEndpoint)
	handleMethod("Join", driver.joinEndpoint)
	handleMethod("Leave", driver.leaveEndpoint)
	var (
		listener net.Listener
		err      error
	)

	listener, err = net.Listen("unix", socket)
	if err != nil {
		return err
	}

	return http.Serve(listener, router)
}

func notFound(w http.ResponseWriter, r *http.Request) {
	log.Warnf("plugin Not found: [ %+v ]", r)
	http.NotFound(w, r)
}

func sendError(w http.ResponseWriter, msg string, code int) {
	log.Errorf("%d %s", code, msg)
	http.Error(w, msg, code)
}

func errorResponsef(w http.ResponseWriter, fmtString string, item ...interface{}) {
	json.NewEncoder(w).Encode(map[string]string{
		"Err": fmt.Sprintf(fmtString, item...),
	})
}

func objectResponse(w http.ResponseWriter, obj interface{}) {
	if err := json.NewEncoder(w).Encode(obj); err != nil {
		sendError(w, "Could not JSON encode response", http.StatusInternalServerError)
		return
	}
}

func emptyResponse(w http.ResponseWriter) {
	json.NewEncoder(w).Encode(map[string]string{})
}

type handshakeResp struct {
	Implements []string
}

func (driver *driver) handshake(w http.ResponseWriter, r *http.Request) {
	err := json.NewEncoder(w).Encode(&handshakeResp{
		[]string{"NetworkDriver"},
	})
	if err != nil {
		log.Fatalf("handshake encode: %s", err)
		sendError(w, "encode error", http.StatusInternalServerError)
		return
	}
	log.Debug("Handshake completed")
}

type capabilitiesResp struct {
	Scope string
}

func (driver *driver) capabilities(w http.ResponseWriter, r *http.Request) {
	err := json.NewEncoder(w).Encode(&capabilitiesResp{
		"local",
	})
	if err != nil {
		log.Fatalf("capabilities encode: %s", err)
		sendError(w, "encode error", http.StatusInternalServerError)
		return
	}
	log.Debug("Capabilities exchange complete")
}

type networkCreate struct {
	NetworkID string
	Options   map[string]interface{}
	IpV4Data  []driverapi.IPAMData
	ipV6Data  []driverapi.IPAMData
}

func (driver *driver) createNetwork(w http.ResponseWriter, r *http.Request) {
	var create networkCreate
	err := json.NewDecoder(r.Body).Decode(&create)
	if err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Debugf("Network Create Called: [ %+v ]", create)
	for _, v4 := range create.IpV4Data {
		driver.gateway = v4.Gateway.IP.String()
		driver.cidr = v4.Pool
	}
	driver.networkID = create.NetworkID
	containerCidr := driver.cidr

	n := &network{
		id:        driver.networkID,
		driver:    driver,
		endpoints: endpointTable{},
	}
	// Parse docker network -o opts
	for k, v := range create.Options {
		if k == "com.docker.network.generic" {
			if genericOpts, ok := v.(map[string]interface{}); ok {
				for key, val := range genericOpts {
					log.Debugf("Libnetwork Opts Sent: [ %s ] Value: [ %s ]", key, val)
					// Parse -o mode from libnetwork generic opts
					if key == "mode" {
						switch val {
						case ipVlanL2:
							log.Debugf("Ipvlan mode is L2 [ %s ]", val)
							// backward compatable
							ipVlanMode = ipVlanL2
							// new API
							n.modeOpt = ipVlanL2
						case ipVlanL3:
							log.Debugf("Ipvlan mode is L3 [ %s ]", val)
							// IPVlan simply needs the container interface for its
							// default route target since only unicast is allowed <3
							// backward compatable
							ipVlanMode = ipVlanL3
							// new API
							n.modeOpt = ipVlanL3
						case ipVlanL3Routing:
							// IPVlan simply needs the container interface for its
							// default route target since only unicast is allowed <3
							ipVlanMode = ipVlanL3Routing
							n.modeOpt = ipVlanL3Routing
						}
					}
					// Parse -o host_iface from libnetwork generic opts
					if key == "host_iface" {
						n.ifaceOpt = val.(string)
					}
				}
			}
		}
	}
	driver.addNetwork(n)
	emptyResponse(w)

	if ipVlanMode == ipVlanL3 {
		log.Debugf("Adding route for the local ipvlan subnet [ %s ] in the default namespace using the specified host interface [ %s]", containerCidr.String(), ipVlanEthIface)
		ipvlanParent, err := netlink.LinkByName(ipVlanEthIface)
		// Add a route in the default NS to point to the IPVlan namespace subnet
		addRouteIface(containerCidr, ipvlanParent)
		if err != nil {
			log.Debugf("a problem occurred adding the container subnet default namespace route", err)
		}
	} else if ipVlanMode == ipVlanL3Routing {
		log.Debugf("Adding route for the local ipvlan subnet [ %s ] in the default namespace using the specified host interface [ %s]", containerCidr.String(), ipVlanEthIface)
		ipvlanParent, err := netlink.LinkByName(ipVlanEthIface)
		// Add a route in the default NS to point to the IPVlan namespace subnet
		addRouteIface(containerCidr, ipvlanParent)
		if err != nil {
			log.Debugf("a problem occurred adding the container subnet default namespace route", err)
		}

		// Announce the local IPVLAN network to the other peers in the BGP cluster
		log.Infof("New Docker network: [ %s ]", containerCidr.String())
		err = routing.AdvertizeNewRoute(containerCidr)
		if err != nil {
			log.Fatalf("Error installing container route : %s", err)
		}
	}
}

// addRouteIface required for L3 mode adds a link scoped route in the default ns
func addRouteIface(ipVlanL3Network *net.IPNet, iface netlink.Link) error {
	return netlink.RouteAdd(&netlink.Route{
		LinkIndex: iface.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Dst:       ipVlanL3Network,
	})
}

type networkDelete struct {
	NetworkID string
}

func (driver *driver) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	var delete networkDelete
	nid := delete.NetworkID
	if err := json.NewDecoder(r.Body).Decode(&delete); err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Debugf("Delete network request: %+v", &delete)
	if delete.NetworkID != driver.networkID {
		log.Debugf("network not found: %+v", &delete)
		errorResponsef(w, "Network %s not found", nid)
		return
	}
	driver.networkID = ""
	emptyResponse(w)
	log.Infof("Deleting plugin cache for network %s", nid)

	if driver.networkID == "" {
		log.Errorf("invalid network id")
	}
	n := driver.network(driver.networkID)
	if n == nil {
		log.Errorf("could not find network with id %s in plugin cache", nid)
	}
	driver.delNetwork(nid)
}

// delRouteIface clean up the required L3 mode default ns route
func delRouteIface(ipVlanL3Network *net.IPNet, iface netlink.Link) error {
	return netlink.RouteDel(&netlink.Route{
		LinkIndex: iface.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Dst:       ipVlanL3Network,
	})
}

type endpointCreate struct {
	NetworkID  string
	EndpointID string
	Interface  *EndpointInterface
	Options    map[string]interface{}
}

// EndpointInterface represents an interface endpoint.
type EndpointInterface struct {
	Address     string
	AddressIPv6 string
	MacAddress  string
}

type InterfaceName struct {
	SrcName   string
	DstName   string
	DstPrefix string
}

type endpointResponse struct {
	Interface EndpointInterface
}

func (driver *driver) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var create endpointCreate
	if err := json.NewDecoder(r.Body).Decode(&create); err != nil {
		sendError(w, "Unable to decode JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	netID := create.NetworkID
	endID := create.EndpointID

	if netID != driver.networkID {
		log.Warnf("Network not found, [ %s ]", netID)
		errorResponsef(w, "No such network %s", netID)
		return
	}
	log.Debugf("The container subnet for this context is [ %s ]", create.Interface.Address)
	// Request an IP address from libnetwork based on the cidr scope
	// TODO: Add a user defined static ip addr option
	containerAddress := create.Interface.Address
	if containerAddress == "" {
		log.Errorf("Unable to obtain an IP address from libnetwork default ipam")
		return
	}

	// generate a mac address for the pending container
	mac := makeMac(net.ParseIP(containerAddress))

	log.Infof("Allocated container IP: [ %s ]", containerAddress)
	// IP addrs comes from libnetwork ipam via user 'docker network' parameters
	respIface := &EndpointInterface{
		MacAddress: mac,
	}
	resp := &endpointResponse{
		Interface: *respIface,
	}
	log.Debugf("Create endpoint response: %+v", resp)
	objectResponse(w, resp)
	log.Debugf("Create endpoint %s %+v", endID, resp)
}

type endpointDelete struct {
	NetworkID  string
	EndpointID string
}

func (driver *driver) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	var delete endpointDelete
	if err := json.NewDecoder(r.Body).Decode(&delete); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	log.Debugf("Delete endpoint request: %+v", &delete)
	emptyResponse(w)
	// null check cidr in case driver restarted and doesnt know the network to avoid panic
	if driver.cidr == nil {
		return
	}

	log.Debugf("Delete endpoint %s", delete.EndpointID)

	containerLink := delete.EndpointID[:5]
	// Check the interface to delete exists to avoid a panic if nil
	if ok := validateHostIface(containerLink); !ok {
		log.Errorf("The requested interface to delete [ %s ] was not found on the host.", containerLink)
		return
	}
	// Get the link handle
	link, err := netlink.LinkByName(containerLink)
	if err != nil {
		log.Errorf("Error looking up link [ %s ] object: [ %v ] error: [ %s ]", link.Attrs().Name, link, err)
		return
	}
	log.Infof("Deleting the unused ipvlan link [ %s ] from the removed container", link.Attrs().Name)
	// Delete the link
	if err := netlink.LinkDel(link); err != nil {
		log.Errorf("Unable to delete the ipvlan link named [ %s ] for the exiting container: %s", link.Attrs().Name, err)
	}
}

type endpointInfoReq struct {
	NetworkID  string
	EndpointID string
}

type endpointInfo struct {
	Value map[string]interface{}
}

func (driver *driver) infoEndpoint(w http.ResponseWriter, r *http.Request) {
	var info endpointInfoReq
	if err := json.NewDecoder(r.Body).Decode(&info); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	log.Debugf("Endpoint info request: %+v", &info)
	objectResponse(w, &endpointInfo{Value: map[string]interface{}{}})
	log.Debugf("Endpoint info %s", info.EndpointID)
}

type joinInfo struct {
	InterfaceName *InterfaceName
	Gateway       string
	GatewayIPv6   string
}

type join struct {
	NetworkID  string
	EndpointID string
	SandboxKey string
	Options    map[string]interface{}
}

type staticRoute struct {
	Destination string
	RouteType   int
	NextHop     string
}

type joinResponse struct {
	Gateway       string
	InterfaceName InterfaceName
	StaticRoutes  []*staticRoute
}

func (driver *driver) joinEndpoint(w http.ResponseWriter, r *http.Request) {
	var j join
	if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	log.Debugf("Join request: %+v", &j)

	getID, err := driver.getNetwork(j.NetworkID)
	if err != nil {
		log.Errorf("error getting network ID mode [ %s ]: %v", j.NetworkID, err)
	}

	endID := j.EndpointID
	// unique name while still on the common netns
	preMoveName := endID[:5]
	res := &joinResponse{}

	/* Backward compatable for plugin options */
	if getID.modeOpt == "" {
		mode, err := getIPVlanMode(ipVlanMode)
		if err != nil {
			log.Errorf("error getting vlan mode [ %v ]: %s", mode, err)
			return
		}
		// Get the link for the master index (Example: the docker host eth iface)
		hostEth, err := netlink.LinkByName(ipVlanEthIface)
		if err != nil {
			log.Warnf("Error looking up the parent iface [ %s ] error: [ %s ]", ipVlanEthIface, err)
		}
		ipvlan := &netlink.IPVlan{
			LinkAttrs: netlink.LinkAttrs{
				Name:        preMoveName,
				ParentIndex: hostEth.Attrs().Index,
				TxQLen:      TxQueueLen,
			},
			Mode: mode,
		}
		if err := netlink.LinkAdd(ipvlan); err != nil {
			log.Warnf("Failed to create the netlink link: [ %v ] with the "+
				"error: %s Note: a parent index cannot be link to both ipvlan "+
				"and ipvlan simultaneously. A new parent index is required", ipvlan, err)
			log.Warnf("Also check `/var/run/docker/netns/` for orphaned links to unmount and delete, then restart the plugin")
			log.Warnf("Run this to clean orphaned links 'umount /var/run/docker/netns/* && rm /var/run/docker/netns/*'")
		}
		log.Infof("Created ipvlan link: [ %s ] with a mode: [ %s ]", ipvlan.Name, ipVlanMode)
		// Set the netlink iface MTU, default is 1500
		if err := netlink.LinkSetMTU(ipvlan, defaultMTU); err != nil {
			log.Errorf("Error setting the MTU [ %d ] for link [ %s ]: %s", defaultMTU, ipvlan.Name, err)
		}
		// Bring the netlink iface up
		if err := netlink.LinkSetUp(ipvlan); err != nil {
			log.Warnf("failed to enable the ipvlan netlink link: [ %v ]", ipvlan, err)
		}

		// SrcName gets renamed to DstPrefix on the container iface
		ifname := &InterfaceName{
			SrcName:   ipvlan.Name,
			DstPrefix: containerEthPrefix,
		}

		// L2 ipvlan needs an explicit IP for a default GW in the container netns
		if ipVlanMode == ipVlanL2 {
			res = &joinResponse{
				InterfaceName: *ifname,
				Gateway:       driver.gateway,
			}
			defer objectResponse(w, res)
		}
		// ipvlan L3 mode doesnt need an IP for a default GW, just an iface dex.
		if ipVlanMode == ipVlanL3 || ipVlanMode == ipVlanL3Routing {
			res = &joinResponse{
				InterfaceName: *ifname,
			}
			// Add a default route of only the interface inside the container
			defaultRoute := &staticRoute{
				Destination: defaultRoute,
				RouteType:   types.CONNECTED,
				NextHop:     "",
			}
			res.StaticRoutes = []*staticRoute{defaultRoute}
		}

	} else {
		/* Using Docker -o options parameter */
		mode, err := getIPVlanMode(getID.modeOpt)
		if err != nil {
			log.Errorf("error getting vlan mode [ %v ]: %s", mode, err)
			return
		}

		// Get the link for the master index (Example: the docker host eth iface)
		hostEth, err := netlink.LinkByName(ipVlanEthIface)
		if err != nil {
			log.Warnf("Error looking up the parent iface [ %s ] error: [ %s ]", ipVlanEthIface, err)
		}
		ipvlan := &netlink.IPVlan{
			LinkAttrs: netlink.LinkAttrs{
				Name:        preMoveName,
				ParentIndex: hostEth.Attrs().Index,
				TxQLen:      TxQueueLen,
			},
			Mode: mode,
		}
		if err := netlink.LinkAdd(ipvlan); err != nil {
			log.Warnf("Failed to create the netlink link: [ %v ] with the "+
				"error: %s Note: a parent index cannot be link to both ipvlan "+
				"and ipvlan simultaneously. A new parent index is required", ipvlan, err)
			log.Warnf("Also check `/var/run/docker/netns/` for orphaned links to unmount and delete, then restart the plugin")
			log.Warnf("Run this to clean orphaned links 'umount /var/run/docker/netns/* && rm /var/run/docker/netns/*'")
		}
		log.Infof("Created ipvlan link: [ %s ] with a mode: [ %s ]", ipvlan.Name, ipVlanMode)
		// Set the netlink iface MTU, default is 1500
		if err := netlink.LinkSetMTU(ipvlan, defaultMTU); err != nil {
			log.Errorf("Error setting the MTU [ %d ] for link [ %s ]: %s", defaultMTU, ipvlan.Name, err)
		}
		// Bring the netlink iface up
		if err := netlink.LinkSetUp(ipvlan); err != nil {
			log.Warnf("failed to enable the ipvlan netlink link: [ %v ]", ipvlan, err)
		}
		// SrcName gets renamed to DstPrefix on the container iface
		ifname := &InterfaceName{
			SrcName:   ipvlan.Name,
			DstPrefix: containerEthPrefix,
		}
		// L2 ipvlan needs an explicit IP for a default GW in the container netns
		if ipVlanMode == ipVlanL2 {
			res = &joinResponse{
				InterfaceName: *ifname,
				Gateway:       driver.gateway,
			}
			defer objectResponse(w, res)
		}

		// ipvlan L3 mode doesnt need an IP for a default GW, just an iface dex.
		if ipVlanMode == ipVlanL3 || ipVlanMode == ipVlanL3Routing {
			res = &joinResponse{
				InterfaceName: *ifname,
			}
			// Add a default route of only the interface inside the container
			defaultRoute := &staticRoute{
				Destination: defaultRoute,
				RouteType:   types.CONNECTED,
				NextHop:     "",
			}
			res.StaticRoutes = []*staticRoute{defaultRoute}
		}
	}
	log.Debugf("Join response: %+v", res)
	// Send the response to libnetwork
	objectResponse(w, res)
	log.Debugf("Join endpoint %s:%s to %s", j.NetworkID, j.EndpointID, j.SandboxKey)
}

type leave struct {
	NetworkID  string
	EndpointID string
	Options    map[string]interface{}
}

func (driver *driver) leaveEndpoint(w http.ResponseWriter, r *http.Request) {
	var l leave
	if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
		sendError(w, "Could not decode JSON encode payload", http.StatusBadRequest)
		return
	}
	log.Debugf("Leave request: %+v", &l)
	emptyResponse(w)
	log.Debugf("Leave %s:%s", l.NetworkID, l.EndpointID)
}

func (driver *driver) natOut() error {
	masquerade := []string{
		"POSTROUTING", "-t", "nat",
		"-s", driver.pluginConfig.containerSubnet.String(),
		"-j", "MASQUERADE",
	}
	if _, err := iptables.Raw(
		append([]string{"-C"}, masquerade...)...,
	); err != nil {
		incl := append([]string{"-I"}, masquerade...)
		if output, err := iptables.Raw(incl...); err != nil {
			return err
		} else if len(output) > 0 {
			return &iptables.ChainError{
				Chain:  "POSTROUTING",
				Output: output,
			}
		}
	}
	return nil
}

// return string representation of pluginConfig for debugging
func (d *pluginConfig) String() string {
	str := fmt.Sprintf(" container subnet: [%s],\n", d.containerSubnet.String())
	str = str + fmt.Sprintf("  container gateway: [%s],\n", d.gatewayIP.String())
	str = str + fmt.Sprintf("  host interface: [%s],\n", d.hostIface)
	str = str + fmt.Sprintf("  mmtu: [%d],\n", d.mtu)
	str = str + fmt.Sprintf("  ipvlan mode: [%s]", d.mode)
	return str
}
