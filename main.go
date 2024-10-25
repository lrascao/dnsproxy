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
	"github.com/spf13/viper"
)

type destination struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

type config struct {
	Log struct {
		Level string `yaml:"level"`
	} `yaml:"log"`
	Forward struct {
		Port int `yaml:"port"`
	} `yaml:"forward"`
	Admin struct {
		Port  int    `yaml:"port"`
		Token string `yaml:"token"`
	} `yaml:"admin"`
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

	updateDstCh := make(chan []destination)
	serveHTTP(ctx, cfg, updateDstCh)

	src := fmt.Sprintf(":%d", cfg.Forward.Port)

	forwarder, err := forward.NewForwarder(src,
		forward.WithTimeout(30*time.Second),
		forward.WithConnectCallback(func(addr string) {
			slog.Debug("connected", "from", addr)
		}),
		forward.WithDisconnectCallback(func(addr string) {
			slog.Debug("disconnected", "from", addr)
		}),
	)
	if err != nil {
		panic(err)
	}
	defer forwarder.Close()

	go func() {
		fmt.Printf("forwarding UDP on %s to %v\n",
			src, forwarder.Destinations())
		forwarder.Start(ctx)
	}()

	for {
		select {
		case <-ctx.Done():
		case destinations := <-updateDstCh:
			fmt.Printf("New target destinations: %v\n", destinations)
			var opts []forward.Option
			for _, dst := range destinations {
				opts = append(opts, forward.WithDestination(dst.Name, dst.Addr))
			}
			if err := forwarder.Update(opts...); err != nil {
				log.Error("Error updating forwarder", "error", err)
			}
			fmt.Printf("update: forwarding UDP on %s to %v\n",
				src, forwarder.Destinations())
		}
	}
}

func serveHTTP(ctx context.Context, cfg config, ch chan []destination) {
	mux := http.NewServeMux()
	mux.HandleFunc("/",
		func(w http.ResponseWriter, r *http.Request) {
			// authorize request
			if secret := r.Header.Get("Authorization"); secret != cfg.Admin.Token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// read the whole body into a string
			body, err := ioutil.ReadAll(r.Body)
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
		Addr:    fmt.Sprintf(":%d", cfg.Admin.Port),
		Handler: mux,
		BaseContext: func(l net.Listener) context.Context {
			return ctx
		},
	}

	fmt.Printf("admin HTTP running on :%d\n", cfg.Admin.Port)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("http server closed\n")
		} else if err != nil {
			fmt.Printf("error listening server: %v\n", err)
		}
	}()

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
