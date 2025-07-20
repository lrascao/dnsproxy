package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	forward "github.com/lrascao/udp-forward"
	"github.com/miekg/dns"
	"github.com/spf13/viper"
)

type destination struct {
	Name    string `json:"name"`
	Address string `json:"addr"`
}

type config struct {
	Log struct {
		Level string `yaml:"level"`
	} `yaml:"log"`
	Forward struct {
		Port   int `yaml:"port"`
		Static []struct {
			Name    string `yaml:"name"`
			Address string `yaml:"address"`
		} `yaml:"static,omitempty"`
	} `yaml:"forward"`
	Admin struct {
		Port  int    `yaml:"port"`
		Token string `yaml:"token"`
	} `yaml:"admin"`
	HealthCheck struct {
		Period time.Duration `yaml:"period"`
		Domain string        `yaml:"domain"`
	} `yaml:"healthCheck"`
}

type runner struct {
	cfg          config
	destinations []destination
}

func main() {
	ctx := context.Background()

	// read config argument from command line
	var configFile string
	flag.StringVar(&configFile, "config", "config.yaml", "config file")
	flag.Parse()

	// read config file using viper
	viper.SetConfigFile(configFile)
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			panic(err)
		}
	}

	// unmarshal config file into config struct
	var cfg config
	if err := viper.Unmarshal(&cfg); err != nil {
		panic(err)
	}

	logHandler := slog.NewTextHandler(os.Stdout,
		&slog.HandlerOptions{
			Level:     toLevelDebug(cfg.Log.Level),
			AddSource: true,
		})
	log := slog.New(logHandler)
	slog.SetDefault(log)

	slog.Debug("config", "config", cfg)

	runner := &runner{
		cfg: cfg,
	}
	if err := runner.Run(ctx); err != nil {
		slog.Error("error running runner", "error", err)
		os.Exit(1)
	}
}

func (r *runner) Run(ctx context.Context) error {
	updateDstCh := make(chan []destination)
	if r.cfg.Admin.Port != 0 {
		r.serveHTTP(ctx, updateDstCh)
	}

	opts := []forward.Option{
		forward.WithTimeout(30 * time.Second),
		forward.WithConnectCallback(func(addr string) {
			slog.Debug("connected", "from", addr)
		}),
		forward.WithDisconnectCallback(func(addr string) {
			slog.Debug("disconnected", "from", addr)
		}),
	}

	if len(r.cfg.Forward.Static) != 0 {
		for _, static := range r.cfg.Forward.Static {
			if static.Name == "" || static.Address == "" {
				slog.Warn("skipping static destination with empty name or address",
					"name", static.Name, "addr", static.Address)
				continue
			}
			r.destinations = append(r.destinations,
				destination{
					Name:    static.Name,
					Address: static.Address,
				})
		}
	}

	for _, dst := range r.destinations {
		opts = append(opts,
			forward.WithDestination(
				dst.Name,
				dst.Address,
			))
	}

	src := fmt.Sprintf(":%d", r.cfg.Forward.Port)

	forwarder, err := forward.NewForwarder(src, opts...)
	if err != nil {
		panic(err)
	}
	defer forwarder.Close()

	go func() {
		fmt.Printf("forwarding UDP on %s to %v\n",
			src, forwarder.Destinations())
		forwarder.Start(ctx)
	}()

	if r.cfg.HealthCheck.Period > 0 {
		go func() {
			fmt.Printf("starting health check every %s on domain %s\n",
				r.cfg.HealthCheck.Period, r.cfg.HealthCheck.Domain)

			r.healthCheck(ctx, forwarder)
		}()
	}

	for {
		select {
		case <-ctx.Done():
		case destinations := <-updateDstCh:
			fmt.Printf("New target destinations: %v\n", destinations)
			var opts []forward.Option
			for _, dst := range destinations {
				opts = append(opts,
					forward.WithDestination(
						dst.Name,
						dst.Address),
				)
			}
			if err := forwarder.Update(opts...); err != nil {
				slog.Error("Error updating forwarder", "error", err)
			}
			fmt.Printf("update: forwarding UDP on %s to %v\n",
				src, forwarder.Destinations())
		}
	}
}

func (r *runner) serveHTTP(ctx context.Context, ch chan []destination) {
	mux := http.NewServeMux()
	mux.HandleFunc("/",
		func(w http.ResponseWriter, req *http.Request) {
			// authorize request
			if secret := req.Header.Get("Authorization"); secret != r.cfg.Admin.Token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// read the whole body into a string
			body, err := ioutil.ReadAll(req.Body)
			if err != nil {
				http.Error(w, "error reading body", http.StatusInternalServerError)
				return
			}
			var destinations []destination
			if err := json.Unmarshal(body, &destinations); err != nil {
				http.Error(w, "error parsing body", http.StatusBadRequest)
				return
			}

			ch <- destinations
		})

	// create an http server
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", r.cfg.Admin.Port),
		Handler: mux,
		BaseContext: func(l net.Listener) context.Context {
			return ctx
		},
	}

	fmt.Printf("admin HTTP running on :%d\n", r.cfg.Admin.Port)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("http server closed\n")
		} else if err != nil {
			fmt.Printf("error listening server: %v\n", err)
		}
	}()
}

func (r *runner) healthCheck(ctx context.Context, f forward.Forwarder) {
	ticker := time.NewTicker(r.cfg.HealthCheck.Period)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var healthy []destination
			for _, d := range r.destinations {
				if err := r.checkDNS(ctx, d); err != nil {
					slog.Error(fmt.Sprintf("udp-forward: DNS %s health check failed: %v",
						d.Address, err))
				} else {
					healthy = append(healthy, d)
				}
			}

			var opts []forward.Option
			for _, d := range healthy {
				opts = append(opts,
					forward.WithDestination(
						d.Name,
						d.Address),
				)
			}
			if err := f.Update(opts...); err != nil {
				slog.Error("Error updating forwarder with healthy destinations", "error", err)
			} else {
				slog.Info("Health check completed", "healthy", healthy)
			}
		}
	}
}

func (r *runner) checkDNS(ctx context.Context, d destination) error {
	slog.Debug(fmt.Sprintf("checking DNS %s", d.Address))

	ctx, cancel := context.WithTimeout(ctx, time.Duration(15*time.Second))
	defer cancel()

	// perform a DNS lookup google.com to this specific server to check if
	// it is healthy

	// Create a new DNS message
	m := new(dns.Msg)
	m.SetQuestion(r.cfg.HealthCheck.Domain, dns.TypeA)

	// Create a DNS client
	client := new(dns.Client)
	// Send the query
	reply, _, err := client.Exchange(m, d.Address)
	if err != nil {
		return fmt.Errorf("error querying DNS %s: %w", d.Address, err)
	}
	// Check for response
	if len(reply.Answer) == 0 {
		return fmt.Errorf("no answer received from DNS %s", d.Address)
	}

	return nil
}

func toLevelDebug(lvl string) slog.Level {
	switch lvl {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
