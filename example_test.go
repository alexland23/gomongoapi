package gomongoapi

import (
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// This example shows the minimal setup to stand up a server: build Options,
// point it at a Mongo instance and default database, then Start.
//
// It requires a live MongoDB connection, so it is compiled but not executed
// (no "Output:" comment).
func ExampleNewServer() {
	// Set server options
	serverOpts := ServerOptions()
	serverOpts.SetMongoClientOpts(options.Client().ApplyURI("mongodb://localhost:27017"))
	serverOpts.SetDefaultDB("app")
	serverOpts.SetAddress(":8080") // listen on all interfaces; default is localhost-only, see Security

	// Create server
	server := NewServer(serverOpts)

	// Start server, blocks until SIGINT/SIGTERM triggers a graceful shutdown
	// or an error occurs
	log.Fatal(server.Start())
}

// This example adds a custom GET route under /custom that queries Mongo
// directly via GetMongoClient.
//
// It requires a live MongoDB connection, so it is compiled but not executed
// (no "Output:" comment).
func ExampleServer_AddCustomGET() {
	serverOpts := ServerOptions()
	serverOpts.SetMongoClientOpts(options.Client().ApplyURI("mongodb://localhost:27017"))
	server := NewServer(serverOpts)

	// Add a custom GET route under /custom
	server.AddCustomGET("/appUsersCount", func(ctx *gin.Context) {
		client := server.GetMongoClient()

		count, err := client.Database("app").Collection("users").CountDocuments(ctx.Request.Context(), bson.M{})
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, bson.M{"error": "Error running count: " + err.Error()})
			return
		}

		ctx.JSON(http.StatusOK, bson.M{"Count": count})
	})

	log.Fatal(server.Start())
}
