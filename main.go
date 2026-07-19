package main

import (
	"log"

	"github.com/nuggetplum/VaurdAssignment/db" // Replace with your actual Go module name
)

func main() {
	// This matches the credentials in your docker-compose.yml
	dbURL := "postgres://vaurd_user:vaurd_password@localhost:5432/food_delivery?sslmode=disable"

	dbPool, err := db.InitDB(dbURL)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer dbPool.Close()

	log.Println("Backend service started successfully.")

	// Next step: We will start our HTTP server and Event listeners here!
}
