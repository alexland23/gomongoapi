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

// Give user ability to only do readonly operations
type Server interface {

	// Start server
	Start() error

	SetAPIMiddleware(middleware ...gin.HandlerFunc)
}

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
func NewServer(mongoClientOpts *options.ClientOptions, option *Option) Server {

	router := option.Router

	// Create api route group
	apiRouter := router.Group("/api")

	// Convert limits to string
	findLimit := strconv.Itoa(option.FindLimit)
	findMaxLimit := strconv.Itoa(option.FindMaxLimit)

	return &server{
		mongoClientOpts: mongoClientOpts,
		router:          router,
		apiRouter:       apiRouter,
		address:         option.Address,
		defaultDB:       option.DefaultDB,
		findLimit:       findLimit,
		findMaxLimit:    findMaxLimit,
	}
}

// Sets any middleware funcs for /api routes
// Example use would be any authorization
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

	// Get pipeline
	pipeLine, ok := reqBody["Aggregate"].([]interface{})
	if !ok {
		ctx.String(http.StatusBadRequest, "Request Body is missing aggregate pipeline")
		return
	}

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
