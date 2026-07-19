package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/nuggetplum/VaurdAssignment/broker"
	"github.com/nuggetplum/VaurdAssignment/db"
	"github.com/nuggetplum/VaurdAssignment/server"
)

func main() {
	dbURL := getEnv("DATABASE_URL", "postgres://vaurd_user:vaurd_password@localhost:5432/food_delivery?sslmode=disable")
	natsURL := getEnv("NATS_URL", "nats://localhost:4222")
	httpPort := getEnv("HTTP_PORT", "8080")
	workerCount := getEnvInt("WORKER_COUNT", 8)
	workerBufferSize := getEnvInt("WORKER_BUFFER_SIZE", 64)

	dbPool, err := db.InitDB(dbURL)
	if err != nil {
		log.Fatalf("failed to initialize database: %v", err)
	}
	defer dbPool.Close()

	repo := db.NewRepository(dbPool)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	consumer, err := broker.NewConsumer(ctx, natsURL)
	if err != nil {
		log.Fatalf("failed to connect to broker: %v", err)
	}
	defer consumer.Close()

	workerPool := broker.NewPool(workerCount, workerBufferSize, repo.ApplyEvent)

	consumerErrCh := make(chan error, 1)
	go func() {
		consumerErrCh <- consumer.Run(ctx, workerPool.Submit)
	}()

	httpServer := &http.Server{
		Addr:    ":" + httpPort,
		Handler: server.New(repo).Routes(),
	}

	httpErrCh := make(chan error, 1)
	go func() {
		log.Printf("HTTP server listening on :%s", httpPort)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErrCh <- err
		}
	}()

	// Wait for whichever happens first: a shutdown signal, the consumer
	// exiting on its own, or the HTTP server failing to serve.
	consumerAlreadyDone := false
	select {
	case <-ctx.Done():
		log.Println("shutdown signal received")
	case err := <-consumerErrCh:
		consumerAlreadyDone = true
		if err != nil {
			log.Printf("consumer stopped with error: %v", err)
		}
	case err := <-httpErrCh:
		log.Printf("http server error: %v", err)
	}

	// Idempotent if a signal already triggered this; makes the consumer's
	// fetch loop stop if something else (e.g. an HTTP error) got us here.
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("http server shutdown error: %v", err)
	}

	if !consumerAlreadyDone {
		<-consumerErrCh // wait for the fetch loop to actually stop
	}
	workerPool.Close() // drain in-flight event handling before closing the DB

	log.Println("shutdown complete")
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
