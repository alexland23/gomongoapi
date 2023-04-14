/*
Package mongoapi is a pure go client that allows for easy creation of a server that creates routes to query a MongoDB.
The intent of these routes is to be used alongside either the JSON API or Infinity plugin with Grafana to allow for
MongoDB dashboards within Grafana.

Package is using gin for the server and can be heavily customized as a custom gin engine can be set in the options.

To use the package, user must create the server options and at the minimum set the mongodb client options to connect to
the db. Once the options are made, they can be passed to create a new server. Server Start() function will run the server
and block until it encounters an error.

Example
	// Set server options
	serverOpts := mongoapi.ServerOptions()
	serverOpts.SetMongoClientOpts(options.Client().ApplyURI("mongodb://localhost:27017"))
	serverOpts.SetDefaultDB("app")
	serverOpts.SetAddress(":4004")

	// Create server and set values
	server := mongoapi.NewServer(serverOpts)

	// Start server
	server.Start()

*/
package mongoapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Server interface for mongo api server
type Server interface {

	// Start new server
	// This function will block unless an error occurs
	Start() error

	// Add custom middleware in the /api router group.
	// This allows custom additions like logging, auth, etc
	SetAPIMiddleware(middleware ...gin.HandlerFunc)
}

// Server struct that holds needed fields for server
type server struct {
	// Server fields
	router    *gin.Engine
	apiRouter *gin.RouterGroup
	address   string

	// Mongo fields
	mongoClientOpts *options.ClientOptions
	mongoClient     *mongo.Client
	defaultDB       string
	findLimit       string
	findMaxLimit    string
}

// Create a new server
// Must pass in Mongo Client Options
func NewServer(opts *Options) Server {

	router := opts.Router

	// Create api route group
	apiRouter := router.Group("/api")

	// Convert limits to string
	findLimit := strconv.Itoa(opts.FindLimit)
	findMaxLimit := strconv.Itoa(opts.FindMaxLimit)

	return &server{
		mongoClientOpts: opts.MongoClientOpts,
		router:          router,
		apiRouter:       apiRouter,
		address:         opts.Address,
		defaultDB:       opts.DefaultDB,
		findLimit:       findLimit,
		findMaxLimit:    findMaxLimit,
	}
}

// Add custom middleware in the /api router group.
// This allows custom additions like logging, auth, etc
func (s *server) SetAPIMiddleware(middleware ...gin.HandlerFunc) {
	s.apiRouter.Use(middleware...)
}

// Start new server
// This function will block unless an error occurs
func (s *server) Start() error {

	var err error

	// Create MongoDB Connection
	s.mongoClient, err = mongo.Connect(context.TODO(), s.mongoClientOpts)
	if err != nil {
		return err
	}
	defer func() {
		if err = s.mongoClient.Disconnect(context.TODO()); err != nil {
			log.Printf("Error while disconnecting from MongoDB: %s\n", err.Error())
		}
	}()

	// Test the connection
	err = s.mongoClient.Ping(context.TODO(), nil)
	if err != nil {
		return err
	}

	// Ensure router isnt nil
	if s.router == nil {
		return fmt.Errorf("gin router was is not set")
	}

	// Set routes
	s.createRoutes()

	// Start router, this will block until error occurs
	err = s.router.Run(s.address)

	return err
}

// Sets the routes based on the mongo connection db and collections
func (s *server) createRoutes() {

	// Test connection, always return ok
	s.router.GET("/", func(ctx *gin.Context) {
		ctx.Status(http.StatusOK)
	})

	// Create api group
	s.apiRouter.GET("/databases", s.getDatabases)
	s.apiRouter.GET("/collections", s.getCollections)
	s.apiRouter.POST("/collections/:name/find", s.collectionFind)
	s.apiRouter.POST("/collections/:name/aggregate", s.collectionAggregate)

}

// Route to get all database names
func (s *server) getDatabases(c *gin.Context) {

	// If user set a default database, only return that
	if s.defaultDB != "" {
		res := bson.M{
			"Databases": []string{s.defaultDB},
		}

		c.JSON(http.StatusOK, res)
		return
	}

	dbNames, err := s.mongoClient.ListDatabaseNames(c.Request.Context(), bson.M{})
	if err != nil {
		c.String(http.StatusInternalServerError, "Error getting databases names: %s", err.Error())
		return
	}

	res := bson.M{
		"Databases": dbNames,
	}

	c.JSON(http.StatusOK, res)
}

// Route to get all collection names for the queried database
// /api/collections?database=app
func (s *server) getCollections(c *gin.Context) {

	var dbName string
	// If user didnt set a default db, check to see if one was passed
	if s.defaultDB == "" {
		var ok bool
		dbName, ok = c.GetQuery("database")
		if !ok {
			c.String(http.StatusBadRequest, "Database name was not passed, one is needed")
			return
		}
	} else {
		dbName = s.defaultDB
	}

	collNames, err := s.mongoClient.Database(dbName).ListCollectionNames(c.Request.Context(), bson.M{})
	if err != nil {
		c.String(http.StatusInternalServerError, "Error getting collection names: %s", err.Error())
		return
	}

	res := bson.M{
		"Collections": collNames,
	}

	c.JSON(http.StatusOK, res)
}

// Runs a find on the collection
// /collections/:name/find
// Request body should have the find filter
//	ex) Request Body: {"UserName": "Jon"}
func (s *server) collectionFind(ctx *gin.Context) {

	// If user didn't set a default db, check to see if one was passed
	var dbName string
	if s.defaultDB == "" {
		var ok bool
		dbName, ok = ctx.GetQuery("database")
		if !ok {
			ctx.String(http.StatusBadRequest, "Database name was not passed, one is needed")
			return
		}
	} else {
		dbName = s.defaultDB
	}

	// Get collection name, return error if one isnt passed
	collName := ctx.Param("name")
	if collName == "" {
		ctx.String(http.StatusBadRequest, "Collection name was not passed")
		return
	}

	// Get limit, if none was passed default to default value
	limitString := ctx.DefaultQuery("limit", s.findLimit)
	limit, err := strconv.Atoi(limitString)
	if err != nil {
		ctx.String(http.StatusBadRequest, fmt.Sprintf("Limit is not an int: %s", err.Error()))
		return
	}

	// Get filter from request body
	var filter bson.M
	err = ctx.ShouldBindJSON(&filter)
	if err != nil {
		ctx.String(http.StatusBadRequest, fmt.Sprintf("Error reading body request: %s", err.Error()))
		return
	}

	opts := options.Find()
	opts.SetLimit(int64(limit))
	opts.SetAllowDiskUse(true)

	// Run find
	cursor, err := s.mongoClient.Database(dbName).Collection(collName).Find(ctx.Request.Context(), filter, opts)
	if err != nil {
		ctx.String(http.StatusInternalServerError, "Error running find: %s", err.Error())
		return
	}

	// Decode results
	var res []map[string]interface{}
	err = cursor.All(ctx.Request.Context(), &res)
	if err != nil {
		ctx.String(http.StatusInternalServerError, "Error decoding results: %s", err.Error())
		return
	}

	ctx.JSON(http.StatusOK, res)
}

// Runs an aggregate on the collection
// /collections/:name/aggregate
// Request body should contain the aggregate command
//	ex) Request Body: {"Aggregate": [{"$match": { "UserName": "Jon" }}]
func (s *server) collectionAggregate(ctx *gin.Context) {

	// If user didn't set a default db, check to see if one was passed
	var dbName string
	if s.defaultDB == "" {
		var ok bool
		dbName, ok = ctx.GetQuery("database")
		if !ok {
			ctx.String(http.StatusBadRequest, "Database name was not passed, one is needed")
			return
		}
	} else {
		dbName = s.defaultDB
	}

	// Get collection name, return error if one isnt passed
	collName := ctx.Param("name")
	if collName == "" {
		ctx.String(http.StatusBadRequest, "Collection name was not passed")
		return
	}

	// Get request body
	var reqBody map[string]interface{}
	err := ctx.ShouldBind(&reqBody)
	if err != nil {
		ctx.String(http.StatusBadRequest, fmt.Sprintf("Error reading body request: %s", err.Error()))
		return
	}

	// Get pipeline, if it doesnt exists an empty pipeline will be used
	pipeLine := reqBody["Aggregate"].([]interface{})

	opts := options.Aggregate()
	opts.SetAllowDiskUse(true)

	cursor, err := s.mongoClient.Database(dbName).Collection(collName).Aggregate(ctx.Request.Context(), pipeLine, opts)
	if err != nil {
		ctx.String(http.StatusInternalServerError, "Error running aggregate: %s", err.Error())
		return
	}

	// Decode results
	var res []map[string]interface{}
	err = cursor.All(ctx.Request.Context(), &res)
	if err != nil {
		ctx.String(http.StatusInternalServerError, "Error decoding results: %s", err.Error())
		return
	}

	ctx.JSON(http.StatusOK, res)
}
