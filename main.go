package main

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/chenhw2/https-dns/gdns"
	"github.com/golang/glog"
	"github.com/miekg/dns"
	"github.com/urfave/cli"
)

var (
	version = "MISSING build version [git hash]"

	gdnsOPT   gdns.GDNSOptions
	gdnsEndPT string

	listenAddress   string
	listenProtocols []string
)

func serve(net, addr string) {
	glog.V(LINFO).Infof("starting %s service on %s", net, addr)

	sig := make(chan os.Signal)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	server := &dns.Server{Addr: addr, Net: net, TsigSecret: nil}
	go func() {
		if err := server.ListenAndServe(); err != nil {
			glog.Errorf("Failed to setup the %s server: %s\n", net, err.Error())
			sig <- syscall.SIGTERM
		}
	}()

	// serve until exit
	<-sig

	glog.V(LINFO).Infof("shutting down %s on interrupt\n", net)
	if err := server.Shutdown(); err != nil {
		glog.V(LERROR).Infof("got unexpected error %s", err.Error())
	}
}

func init() {
	// seed the global random number generator, used in some utilities and the
	// google provider
	rand.Seed(time.Now().UTC().UnixNano())
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

func main() {
	app := cli.NewApp()
	app.Name = "https-dns"
	app.Usage = "A DNS-protocol proxy for Google's DNS-over-HTTPS service."
	app.Version = fmt.Sprintf("Git:[%s] (%s)", strings.ToUpper(version), runtime.Version())
	// app.HideVersion = true
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "listen, l",
			Value: ":5300",
			Usage: "Serve address",
		},
		cli.StringFlag{
			Name:  "proxy, p",
			Usage: "Proxy (SOCKS or SHADOWSOCKS) Server for HTTP GET",
		},
		cli.StringFlag{
			Name:  "endpoint, ep",
			Value: "Google",
			Usage: "Google or Cloudflare",
		},
		cli.StringFlag{
			Name:  "endpoint-uri, epuri",
			Usage: "DNS-over-HTTPS endpoint url",
		},
		cli.StringSliceFlag{
			Name:  "endpoint-ip, epip",
			Usage: "IPs of the DNS-over-HTTPS endpoint; if provided, endpoint lookup skip",
		},
		cli.StringSliceFlag{
			Name:  "dns-servers, d",
			Usage: "DNS Servers used to look up the endpoint; system default is used if absent.",
		},
		cli.StringFlag{
			Name:  "edns, e",
			Usage: "Extension mechanisms for DNS (EDNS) is parameters of the Domain Name System (DNS) protocol.",
		},
		cli.BoolFlag{
			Name:  "no-pad, N",
			Usage: "Disable padding of Google DNS-over-HTTPS requests to identical length",
		},
		cli.BoolFlag{
			Name:  "insecure, I",
			Usage: "Disable SSL/TLS Certificate check (for some OS without ca-certificates)",
		},
		cli.BoolFlag{
			Name:  "udp, U",
			Usage: "Listen on UDP",
		},
		cli.BoolFlag{
			Name:  "tcp, T",
			Usage: "Listen on TCP",
		},
	}
	app.Action = func(c *cli.Context) error {
		glogGangstaShim(c)
		listenAddress = c.String("listen")
		if c.Bool("tcp") {
			listenProtocols = append(listenProtocols, "tcp")
		}
		if c.Bool("udp") {
			listenProtocols = append(listenProtocols, "udp")
		}
		if 0 == len(listenProtocols) {
			cli.ShowAppHelp(c)
			os.Exit(0)
		}

		if !strings.HasPrefix(version, "MISSING") {
			fmt.Fprintf(os.Stderr, "%s %s\n", strings.ToUpper(c.App.Name), c.App.Version)
		}

		gdnsOPT.PROXY = c.String("proxy")
		gdnsOPT.EDNS = c.String("edns")
		gdnsOPT.Pad = !c.Bool("no-pad")
		gdnsOPT.Secure = !c.Bool("insecure")

		for _, eip := range c.StringSlice("endpoint-ip") {
			if ip := net.ParseIP(eip); ip == nil {
				glog.V(LERROR).Infof("%+v", fmt.Errorf("unable to parse IP from string %s", eip))
			} else {
				gdnsOPT.EndpointIPs = append(gdnsOPT.EndpointIPs, ip)
			}
		}
		glog.V(LDEBUG).Infof("EndpointIPs%+v", gdnsOPT.EndpointIPs)

		for _, dns := range c.StringSlice("dns-servers") {
			if d, err := gdns.ParseEndpoint(dns, 53); err != nil {
				glog.V(LERROR).Infof("%+v", err)
			} else {
				gdnsOPT.DNSServers = append(gdnsOPT.DNSServers, d)
			}
		}
		glog.V(LDEBUG).Infof("DNSServers%+v", gdnsOPT.DNSServers)

		gdnsEndPT = c.String("endpoint-uri")
		if 0 == len(gdnsEndPT) {
			switch strings.ToUpper(c.String("endpoint")) {
			default:
				fallthrough
			case "GOOGLE":
				gdnsEndPT = `https://dns.google.com/resolve`
			case "CLOUDFLARE":
				gdnsEndPT = `https://cloudflare-dns.com/dns-query?ct=application/dns-json`
				if 0 == len(gdnsOPT.EndpointIPs) {
					gdnsOPT.EndpointIPs = []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("1.0.0.1")}
				}
			}
		}
		return nil
	}
	app.Flags = append(app.Flags, glogGangstaFlags...)
	app.Run(os.Args)
	defer glog.Flush()

	provider, err := gdns.NewGDNSProvider(gdnsEndPT, &gdnsOPT)
	if err != nil {
		glog.Exitln(err)
	}
	// options := &gdns.HandlerOptions{}
	handler := gdns.NewHandler(provider, new(gdns.HandlerOptions))
	dns.HandleFunc(".", handler.Handle)

	// start the servers
	servers := make(chan bool)
	for _, protocol := range listenProtocols {
		go func(protocol string) {
			serve(protocol, listenAddress)
			servers <- true
		}(protocol)
	}

	// wait for servers to exit
	for range listenProtocols {
		<-servers
	}

	glog.V(LINFO).Infoln("servers exited, stopping")
}
