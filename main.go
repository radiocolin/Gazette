package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Gmail struct {
		ClientID        string `yaml:"client_id"`
		ClientSecret    string `yaml:"client_secret"`
		PublicURL       string `yaml:"public_url"`
		CredentialsFile string `yaml:"credentials_file"`
		TokenFile       string `yaml:"token_file"`
		Label           string `yaml:"label"`
		PollingInterval int    `yaml:"polling_interval"`
	} `yaml:"gmail"`
	Server struct {
		Port int    `yaml:"port"`
		User string `yaml:"user"`
		Pass string `yaml:"pass"`
	} `yaml:"server"`
}

var (
	config Config
	cache  *Cache
	mu     sync.RWMutex
)

func loadConfig() error {
	f, err := os.Open("config.yaml")
	if err != nil {
		return err
	}
	defer f.Close()
	return yaml.NewDecoder(f).Decode(&config)
}

func main() {
	if err := loadConfig(); err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	cache = NewCache()

	ctx := context.Background()
	
	initGmail(ctx)
	go pollGmail(ctx)

	mux := http.NewServeMux()
	registerGReaderHandlers(mux)
	registerAuthHandlers(mux)

	// Logging middleware
	loggingMux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("INCOMING REQUEST: %s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", config.Server.Port),
		Handler: loggingMux,
	}

	go func() {
		log.Printf("Starting server on :%d", config.Server.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Listen: %s\n", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	ctxShutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctxShutdown); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Println("Server exiting")
}
