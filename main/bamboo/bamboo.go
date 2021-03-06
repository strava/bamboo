package main

import (
	"bufio"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/go-martini/martini"
	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/kardianos/osext"
	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/natefinch/lumberjack"
	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/samuel/go-zookeeper/zk"
	"github.com/QubitProducts/bamboo/api"
	"github.com/QubitProducts/bamboo/configuration"
	"github.com/QubitProducts/bamboo/qzk"
	"github.com/QubitProducts/bamboo/services/event_bus"
)

/*
	Commandline arguments
*/
var configFilePath string
var logPath string
var serverBindPort string

func init() {
	flag.StringVar(&configFilePath, "config", "config/development.json", "Full path of the configuration JSON file")
	flag.StringVar(&logPath, "log", "", "Log path to a file. Default logs to stdout")
	flag.StringVar(&serverBindPort, "bind", ":8000", "Bind HTTP server to a specific port")
}

func main() {
	flag.Parse()
	configureLog()

	// Load configuration
	conf, err := configuration.FromFile(configFilePath)
	if err != nil {
		log.Fatal(err)
	}

	eventBus := event_bus.New()

	// Wait for died children to avoid zombies
	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGCHLD)
	go func() {
		for {
			sig := <-signalChannel
			if sig == syscall.SIGCHLD {
				r := syscall.Rusage{}
				syscall.Wait4(-1, nil, 0, &r)
			}
		}
	}()

	// Create StatsD client
	conf.StatsD.CreateClient()

	// Create Zookeeper connection
	zkConn := listenToZookeeper(conf, eventBus)

	// Register handlers
	handlers := event_bus.Handlers{Conf: &conf, Zookeeper: zkConn}
	eventBus.Register(handlers.MarathonEventHandler)
	eventBus.Register(handlers.ServiceEventHandler)
	eventBus.Publish(event_bus.MarathonEvent{EventType: "bamboo_startup", Timestamp: time.Now().Format(time.RFC3339)})

	// Handle gracefully exit
	registerOSSignals(&conf, eventBus)

	// Start server
	initServer(&conf, zkConn, eventBus)
}

func initServer(conf *configuration.Configuration, conn *zk.Conn, eventBus *event_bus.EventBus) {
	stateAPI := api.StateAPI{Config: conf, Zookeeper: conn}
	serviceAPI := api.ServiceAPI{Config: conf, Zookeeper: conn}
	eventSubAPI := api.EventSubscriptionAPI{Conf: conf, EventBus: eventBus}

	conf.StatsD.Increment(1.0, "restart", 1)
	// Status live information
	router := martini.Classic()
	router.Get("/status", api.HandleStatus)

	// API
	router.Group("/api", func(api martini.Router) {
		// State API
		api.Get("/state", stateAPI.Get)
		// Service API
		api.Get("/services", serviceAPI.All)
		api.Post("/services", serviceAPI.Create)
		api.Put("/services/**", serviceAPI.Put)
		api.Delete("/services/**", serviceAPI.Delete)
		api.Post("/marathon/event_callback", eventSubAPI.Callback)
	})

	// Static pages
	router.Use(martini.Static(path.Join(executableFolder(), "webapp")))

	if conf.Marathon.UseEventStream {
		// Listen events stream from Marathon
		listenToMarathonEventStream(conf, eventSubAPI)
	} else {
		registerMarathonEvent(conf)
	}
	router.RunOnAddr(serverBindPort)
}

// Get current executable folder path
func executableFolder() string {
	folderPath, err := osext.ExecutableFolder()
	if err != nil {
		log.Fatal(err)
	}
	return folderPath
}

func registerMarathonEvent(conf *configuration.Configuration) {

	client := &http.Client{}
	// it's safe to register with multiple marathon nodes
	for _, marathon := range conf.Marathon.Endpoints() {
		url := marathon + "/v2/eventSubscriptions?callbackUrl=" + conf.Bamboo.Endpoint + "/api/marathon/event_callback"
		req, _ := http.NewRequest("POST", url, nil)
		req.Header.Add("Content-Type", "application/json")
		if len(conf.Marathon.User) > 0 && len(conf.Marathon.Password) > 0 {
			req.SetBasicAuth(conf.Marathon.User, conf.Marathon.Password)
		}
		resp, err := client.Do(req)
		if err != nil {
			errorMsg := "An error occurred while accessing Marathon callback system: %s\n"
			log.Printf(errorMsg, err)
			return
		}
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Fatal(err)
			return
		}
		body := string(bodyBytes)
		if strings.HasPrefix(body, "{\"message") {
			warningMsg := "Access to the callback system of Marathon seems to be failed, response: %s\n"
			log.Printf(warningMsg, body)
		}
	}
}

func createAndListen(conf configuration.Zookeeper) (chan zk.Event, *zk.Conn) {
	conn, _, err := zk.Connect(conf.ConnectionString(), time.Second*10)

	if err != nil {
		log.Panic(err)
	}

	ch, _ := qzk.ListenToConn(conn, conf.Path, true, conf.Delay())
	return ch, conn
}

func listenToZookeeper(conf configuration.Configuration, eventBus *event_bus.EventBus) *zk.Conn {
	serviceCh, serviceConn := createAndListen(conf.Bamboo.Zookeeper)

	go func() {
		for {
			select {
			case _ = <-serviceCh:
				eventBus.Publish(event_bus.ServiceEvent{EventType: "change"})
			}
		}
	}()
	return serviceConn
}

func listenToMarathonEventStream(conf *configuration.Configuration, sub api.EventSubscriptionAPI) {
	client := &http.Client{}
	client.Timeout = 0 * time.Second

	ticker := time.NewTicker(1 * time.Second)
	go func() {
		for _ = range ticker.C {
			for _, marathon := range conf.Marathon.Endpoints() {
				eventsURL := marathon + "/v2/events"
				req, err := http.NewRequest("GET", eventsURL, nil)
				req.Header.Set("Accept", "text/event-stream")
				if len(conf.Marathon.User) > 0 && len(conf.Marathon.Password) > 0 {
					req.SetBasicAuth(conf.Marathon.User, conf.Marathon.Password)
				}
				if err != nil {
					errorMsg := "An error occurred while creating request to Marathon events system: %s\n"
					log.Printf(errorMsg, err)
					continue
				}

				resp, err := client.Do(req)
				if err != nil {
					errorMsg := "An error occurred while making a request to Marathon events system: %s\n"
					log.Printf(errorMsg, err)
					continue
				}

				defer resp.Body.Close()

				reader := bufio.NewReader(resp.Body)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						if err != io.EOF {
							errorMsg := "An error occurred while reading Marathon event: %s\n"
							log.Printf(errorMsg, err)
						}
						break
					}

					if len(strings.TrimSpace(line)) == 0 {
						continue
					}

					if !strings.HasPrefix(line, "data: ") {
						errorMsg := "Wrong event format: %s\n"
						log.Printf(errorMsg, line)
						continue
					}

					line = line[6:]
					sub.Notify([]byte(line))
				}

				log.Println("Event stream connection was closed. Re-opening...")
			}
		}
	}()
}

func configureLog() {
	if len(logPath) > 0 {
		log.SetOutput(io.MultiWriter(&lumberjack.Logger{
			Filename: logPath,
			// megabytes
			MaxSize:    100,
			MaxBackups: 3,
			//days
			MaxAge: 28,
		}, os.Stdout))
	}
}

func registerOSSignals(conf *configuration.Configuration, eventBus *event_bus.EventBus) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		if sig == os.Interrupt {
			log.Println("Server stopping now")
			os.Exit(0)
		} else if sig == syscall.SIGTERM {
			log.Println("Server gracefully exiting...")

			// First disable event bus, so that haproxy won't be
			// restarted while we are trying to shutdown.
			eventBus.Shutdown()

			// Then gracefully shutdown haproxy, blocks
			err := event_bus.ExecCommand(conf.HAProxy.ShutdownCommand)
			if err != nil {
				log.Fatalf("HAProxy: graceful shutdown failed\n")
			} else {
				log.Println("HAProxy: graceful shutdown complete")
			}
			log.Println("Server stopping after graceful haproxy shutdown")
			os.Exit(0)
		}
	}()
}
