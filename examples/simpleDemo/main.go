package main

import (
	"context"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	"github.com/alexland23/gomongoapi"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const dbName = "app"

// Logs is the shape of the sample documents seeded into the "logs"
// collection so the Grafana dashboards have something to query on first run.
type Logs struct {
	TimeStamp time.Time
	Amount    float64
	Country   string
	Word      string
	Count     int
}

func main() {
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}

	if err := seedSampleData(mongoURI); err != nil {
		log.Fatalf("failed to seed sample data: %v", err)
	}

	// Set server options
	serverOpts := gomongoapi.ServerOptions()
	serverOpts.SetMongoClientOpts(options.Client().ApplyURI(mongoURI))
	serverOpts.SetDefaultDB(dbName)
	// Listen on all interfaces so Docker's port publishing (docker-compose.yml) can reach
	// this container; gomongoapi defaults to localhost-only, see README Security section.
	serverOpts.SetAddress(":8080")

	// Create server
	server := gomongoapi.NewServer(serverOpts)

	// Add custom route
	// Route will always return the count of the number of records in the users collection
	server.AddCustomGET("/appUsersCount", func(ctx *gin.Context) {
		client := server.GetMongoClient()

		count, err := client.Database(dbName).Collection("users").CountDocuments(ctx.Request.Context(), bson.M{})
		if err != nil {
			ctx.String(http.StatusInternalServerError, "Error running count: "+err.Error())
			return
		}

		ctx.JSON(http.StatusOK, bson.M{"Count": count})
	})

	// Start server, blocks until SIGINT/SIGTERM triggers a graceful shutdown
	// or an error occurs
	log.Fatal(server.Start())
}

// seedSampleData inserts sample "logs" and "users" documents so the demo has
// data to query right away. It connects on its own (independent of the
// gomongoapi server) and seeds each collection independently, skipping any
// collection that is already non-empty.
func seedSampleData(mongoURI string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(mongoURI))
	if err != nil {
		return err
	}
	defer client.Disconnect(context.Background())

	if err = client.Ping(ctx, nil); err != nil {
		return err
	}

	logs := client.Database(dbName).Collection("logs")

	logsExisting, err := logs.CountDocuments(ctx, bson.M{})
	if err != nil {
		return err
	}
	if logsExisting == 0 {
		countries := []string{"US", "CA", "MX", "BR", "GB", "DE", "FR", "JP"}
		words := []string{"alpha", "bravo", "charlie", "delta", "echo"}

		now := time.Now()
		logDocs := make([]any, 0, 200)
		for i := range 200 {
			logDocs = append(logDocs, Logs{
				TimeStamp: now.Add(-time.Duration(i) * time.Hour),
				Amount:    rand.Float64() * 1000,
				Country:   countries[rand.Intn(len(countries))],
				Word:      words[rand.Intn(len(words))],
				Count:     rand.Intn(100),
			})
		}
		if _, err = logs.InsertMany(ctx, logDocs); err != nil {
			return err
		}
	}

	users := client.Database(dbName).Collection("users")

	usersExisting, err := users.CountDocuments(ctx, bson.M{})
	if err != nil {
		return err
	}
	if usersExisting == 0 {
		userDocs := []any{
			bson.M{"name": "Ada Lovelace", "country": "GB"},
			bson.M{"name": "Grace Hopper", "country": "US"},
			bson.M{"name": "Katherine Johnson", "country": "US"},
		}
		if _, err = users.InsertMany(ctx, userDocs); err != nil {
			return err
		}
	}

	return nil
}
