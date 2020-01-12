package server

import (
	"blocky/config"
	"blocky/resolver"
	"os"
	"os/signal"
	"syscall"

	"blocky/util"
	"fmt"
	"net"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

type Server struct {
	udpServer     *dns.Server
	tcpServer     *dns.Server
	queryResolver resolver.Resolver
}

func logger() *logrus.Entry {
	return logrus.WithField("prefix", "server")
}

func NewServer(cfg *config.Config) (*Server, error) {
	udpHandler := dns.NewServeMux()
	tcpHandler := dns.NewServeMux()
	udpServer := &dns.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Net:     "udp",
		Handler: udpHandler,
		NotifyStartedFunc: func() {
			logger().Infof("udp server is up and running")
		},
		UDPSize: 65535}
	tcpServer := &dns.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Net:     "tcp",
		Handler: tcpHandler,
		NotifyStartedFunc: func() {
			logger().Infof("tcp server is up and running")
		},
	}

	var queryResolver resolver.Resolver

	clientNamesResolver := resolver.NewClientNamesResolver(cfg.ClientLookup)
	queryLoggingResolver := resolver.NewQueryLoggingResolver(cfg.QueryLog)
	conditionalUpstreamResolver := resolver.NewConditionalUpstreamResolver(cfg.Conditional)
	customDNSResolver := resolver.NewCustomDNSResolver(cfg.CustomDNS)
	blacklistResolver := resolver.NewBlockingResolver(cfg.Blocking)

	cachingResolver := resolver.NewCachingResolver()
	parallelUpstreamResolver := createParallelUpstreamResolver(cfg.Upstream.ExternalResolvers)

	clientNamesResolver.Next(queryLoggingResolver)
	queryLoggingResolver.Next(conditionalUpstreamResolver)
	conditionalUpstreamResolver.Next(customDNSResolver)
	customDNSResolver.Next(blacklistResolver)
	blacklistResolver.Next(cachingResolver)
	cachingResolver.Next(parallelUpstreamResolver)

	queryResolver = clientNamesResolver

	server := Server{
		udpServer:     udpServer,
		tcpServer:     tcpServer,
		queryResolver: queryResolver,
	}

	server.printConfiguration()

	udpHandler.HandleFunc(".", server.OnRequest)
	tcpHandler.HandleFunc(".", server.OnRequest)

	return &server, nil
}

func (s *Server) printConfiguration() {
	logger().Info("current configuration:")

	res := s.queryResolver
	for res != nil {
		logger().Infof("-> resolver: '%s'", res)

		for _, c := range res.Configuration() {
			logger().Infof("     %s", c)
		}

		if c, ok := res.(resolver.ChainedResolver); ok {
			res = c.GetNext()
		} else {
			break
		}
	}
}

func createParallelUpstreamResolver(upstream []config.Upstream) resolver.Resolver {
	if len(upstream) == 1 {
		return resolver.NewUpstreamResolver(upstream[0])
	}

	resolvers := make([]resolver.Resolver, len(upstream))

	for i, u := range upstream {
		resolvers[i] = resolver.NewUpstreamResolver(u)
	}

	return resolver.NewParallelBestResolver(resolvers)
}

func (s *Server) Start() {
	logger().Info("Starting server")

	go func() {
		if err := s.udpServer.ListenAndServe(); err != nil {
			logger().Fatalf("start %s listener failed: %v", s.udpServer.Net, err)
		}
	}()

	go func() {
		if err := s.tcpServer.ListenAndServe(); err != nil {
			logger().Fatalf("start %s listener failed: %v", s.tcpServer.Net, err)
		}
	}()

	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGUSR1)

	go func() {
		for {
			<-signals
			s.printConfiguration()
		}
	}()
}

func (s *Server) Stop() {
	logger().Info("Stopping server")

	if err := s.udpServer.Shutdown(); err != nil {
		logger().Fatalf("stop %s listener failed: %v", s.udpServer.Net, err)
	}

	if err := s.tcpServer.Shutdown(); err != nil {
		logger().Fatalf("stop %s listener failed: %v", s.tcpServer.Net, err)
	}
}

func (s *Server) OnRequest(w dns.ResponseWriter, request *dns.Msg) {
	logger().Debug("new request")

	clientIP := resolveClientIP(w.RemoteAddr())
	r := &resolver.Request{
		ClientIP: clientIP,
		Req:      request,
		Log: logrus.WithFields(logrus.Fields{
			"question":  util.QuestionToString(request.Question),
			"client_ip": clientIP,
		}),
	}

	response, err := s.queryResolver.Resolve(r)

	if err != nil {
		logger().Errorf("error on processing request: %v", err)
		dns.HandleFailed(w, request)
	} else {
		response.Res.MsgHdr.RecursionAvailable = request.MsgHdr.RecursionDesired

		if err := w.WriteMsg(response.Res); err != nil {
			logger().Error("can't write message: ", err)
		}
	}
}

func resolveClientIP(addr net.Addr) net.IP {
	var clientIP net.IP
	if t, ok := addr.(*net.UDPAddr); ok {
		clientIP = t.IP
	} else if t, ok := addr.(*net.TCPAddr); ok {
		clientIP = t.IP
	}

	return clientIP
}