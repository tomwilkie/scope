package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/weaveworks/procspy"
	"github.com/weaveworks/scope/probe/docker"
	"github.com/weaveworks/scope/probe/endpoint"
	"github.com/weaveworks/scope/probe/host"
	"github.com/weaveworks/scope/probe/process"
	"github.com/weaveworks/scope/probe/tag"
	"github.com/weaveworks/scope/report"
	"github.com/weaveworks/scope/xfer"
)

var version = "dev" // set at build time

const linux = "linux" // runtime.GOOS

func main() {
	var (
		httpListen         = flag.String("http.listen", "", "listen address for HTTP profiling and instrumentation server")
		publishInterval    = flag.Duration("publish.interval", 3*time.Second, "publish (output) interval")
		spyInterval        = flag.Duration("spy.interval", time.Second, "spy (scan) interval")
		listen             = flag.String("listen", ":"+strconv.Itoa(xfer.ProbePort), "listen address")
		prometheusEndpoint = flag.String("prometheus.endpoint", "/metrics", "Prometheus metrics exposition endpoint (requires -http.listen)")
		spyProcs           = flag.Bool("processes", true, "report processes (needs root)")
		dockerEnabled      = flag.Bool("docker", true, "collect Docker-related attributes for processes")
		dockerInterval     = flag.Duration("docker.interval", 10*time.Second, "how often to update Docker attributes")
		dockerBridge       = flag.String("docker.bridge", "docker0", "the docker bridge name")
		weaveRouterAddr    = flag.String("weave.router.addr", "", "IP address or FQDN of the Weave router")
		procRoot           = flag.String("proc.root", "/proc", "location of the proc filesystem")
	)
	flag.Parse()

	if len(flag.Args()) != 0 {
		flag.Usage()
		os.Exit(1)
	}

	log.Printf("probe version %s", version)

	procspy.SetProcRoot(*procRoot)

	if *httpListen != "" {
		log.Printf("profiling data being exported to %s", *httpListen)
		log.Printf("go tool pprof http://%s/debug/pprof/{profile,heap,block}", *httpListen)
		if *prometheusEndpoint != "" {
			log.Printf("exposing Prometheus endpoint at %s%s", *httpListen, *prometheusEndpoint)
			http.Handle(*prometheusEndpoint, makePrometheusHandler())
		}
		go func(err error) { log.Print(err) }(http.ListenAndServe(*httpListen, nil))
	}

	if *spyProcs && os.Getegid() != 0 {
		log.Printf("warning: process reporting enabled, but that requires root to find everything")
	}

	publisher, err := xfer.NewTCPPublisher(*listen)
	if err != nil {
		log.Fatal(err)
	}
	defer publisher.Close()

	var (
		hostName = hostname()
		hostID   = hostName // TODO: we should sanitize the hostname
	)

	var (
		weaveTagger *tag.WeaveTagger
	)

	taggers := []tag.Tagger{
		tag.NewTopologyTagger(),
		tag.NewOriginHostTagger(hostID),
	}

	reporters := []tag.Reporter{
		host.NewReporter(hostID, hostName),
		endpoint.NewReporter(hostID, hostName, *spyProcs),
	}

	if *dockerEnabled && runtime.GOOS == linux {
		if err = report.AddLocalBridge(*dockerBridge); err != nil {
			log.Fatalf("failed to get docker bridge address: %v", err)
		}

		dockerRegistry, err := docker.NewRegistry(*dockerInterval)
		if err != nil {
			log.Fatalf("failed to start docker registry: %v", err)
		}
		defer dockerRegistry.Stop()

		taggers = append(taggers, docker.NewTagger(dockerRegistry, *procRoot))
		reporters = append(reporters, docker.NewReporter(dockerRegistry, hostID))
	}

	if *weaveRouterAddr != "" {
		var err error
		weaveTagger, err = tag.NewWeaveTagger(*weaveRouterAddr)
		if err != nil {
			log.Fatalf("failed to start Weave tagger: %v", err)
		}
		taggers = append(taggers, weaveTagger)
	}

	// TODO provide an alternate implementation for Darwin.
	if runtime.GOOS == linux {
		reporters = append(reporters, process.NewReporter(*procRoot, hostID))
	}

	log.Printf("listening on %s", *listen)

	quit := make(chan struct{})
	defer close(quit)
	go func() {
		var (
			pubTick = time.Tick(*publishInterval)
			spyTick = time.Tick(*spyInterval)
			r       = report.MakeReport()
		)

		for {
			select {
			case <-pubTick:
				publishTicks.WithLabelValues().Add(1)
				publisher.Publish(r)
				r = report.MakeReport()

			case <-spyTick:
				for _, reporter := range reporters {
					newReport, err := reporter.Report()
					if err != nil {
						log.Printf("error generating report: %v", err)
					}
					r.Merge(newReport)
				}

				if weaveTagger != nil {
					r.Overlay.Merge(weaveTagger.OverlayTopology())
				}

				r = tag.Apply(r, taggers)

			case <-quit:
				return
			}
		}
	}()

	log.Printf("%s", <-interrupt())
}

func interrupt() chan os.Signal {
	c := make(chan os.Signal)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	return c
}
