/*
Package gomongoapi is a pure go client that allows for easy creation of a server that creates routes to query a MongoDB.
The intent of these routes is to be used alongside either the JSON API or Infinity plugin with Grafana to allow for
MongoDB dashboards within Grafana.

Package is using gin for the server and can be heavily customized as a custom gin engine can be set in the options.

Available default routes:

	+----------------------------------+-----------+-------+------------------------------------------------------------------------------------------------------+
	| Path                             | HTTP Verb | Body  | Result                                                                                               |
	+----------------------------------+-----------+-------+------------------------------------------------------------------------------------------------------+
	| /                                |    GET    | Empty | Always 200, test connection.                                                                         |
	| /api/databases                   |    GET    | Empty | Returns list of available databases, unless a default is set.                                        |
	| /api/collections                 |    GET    | Empty | Returns a list collections to the default db or the one passed in url param.                         |
	| /api/collections/:name/find      |    POST   | JSON  | Returns result of find on the collection name. DB is either default or one passed in url param.      |
	| /api/collections/:name/count     |    POST   | JSON  | Returns count of documents in the collection name. DB is either default or one passed in url param.  |
	| /api/collections/:name/aggregate |    POST   | JSON  | Returns result of aggregate on the collection name. DB is either default or one passed in url param. |
	| /custom/<Custom Route>           |    GET    | N/A   | Users can create custom GET route, they control everything.                                          |
	| /custom/<Custom Route>           |    POST   | N/A   | Users can create custom POST route, they control everything.                                         |
	+----------------------------------+-----------+-------+------------------------------------------------------------------------------------------------------+

To use the package, user must create the server options and at the minimum set the mongodb client options to connect to
the db. Once the options are made, they can be passed to create a new server. Server Start() function will run the server
and block until a SIGINT/SIGTERM is received or it encounters an error, gracefully shutting down before returning.

Example

	// Set server options
	serverOpts := gomongoapi.ServerOptions()
	serverOpts.SetMongoClientOpts(options.Client().ApplyURI("mongodb://localhost:27017"))
	serverOpts.SetDefaultDB("app")
	serverOpts.SetAddress(":8080")

	// Create server and set values
	server := gomongoapi.NewServer(serverOpts)

	// Add custom route
	// Route will always return the count of the number of records in users collection
	server.AddCustomGET("/appUsersCount", func(ctx *gin.Context) {
		client := server.GetMongoClient()

		count, err := client.Database("app").Collection("users").CountDocuments(ctx.Request.Context(), bson.M{})
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, bson.M{"error": "Error running count: " + err.Error()})
			return
		}

		ctx.JSON(http.StatusOK, bson.M{"Count": count})
	})

	// Start server
	server.Start()
*/
package gomongoapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// defaultConnectTimeout bounds how long Start waits for the initial Mongo
// Connect+Ping before giving up, so a hung Mongo host can't block startup
// indefinitely.
const defaultConnectTimeout = 10 * time.Second

// defaultShutdownTimeout bounds how long graceful HTTP shutdown and the
// subsequent Mongo disconnect are each allowed to take once a shutdown
// signal is received.
const defaultShutdownTimeout = 10 * time.Second

// defaultReadHeaderTimeout bounds how long the HTTP server waits to read a
// request's headers, guarding against slow-header (Slowloris) attacks.
const defaultReadHeaderTimeout = 5 * time.Second

// Server interface for mongo api server
type Server interface {

	// Start new server
	// This function blocks until a SIGINT/SIGTERM is received or an error
	// occurs, at which point it shuts down the HTTP server and disconnects
	// from MongoDB before returning.
	Start() error

	// Add custom middleware in the /api router group.
	// This allows custom additions like logging, auth, etc
	SetAPIMiddleware(middleware ...gin.HandlerFunc)

	// Add custom middleware in the /custom router group.
	// This allows custom additions like logging, auth, etc
	SetCustomMiddleware(middleware ...gin.HandlerFunc)

	// Add custom GET request, path will be under the /custom route group
	AddCustomGET(relativePath string, handlers ...gin.HandlerFunc)

	// Add custom POST request, path will be under the /custom route group
	AddCustomPOST(relativePath string, handlers ...gin.HandlerFunc)

	// Returns server mongo client.
	// This can be used along side AddCustomGET() and AddCustomPost() to make custom routes that use the db.
	GetMongoClient() *mongo.Client
}

// Server struct that holds needed fields for server
type server struct {
	// Server fields
	router       *gin.Engine
	apiRouter    *gin.RouterGroup
	customRouter *gin.RouterGroup
	address      string

	// Mongo fields
	mongoClientOpts *options.ClientOptions
	mongoClient     *mongo.Client
	defaultDB       string
	findLimit       string
	maxLimit        int

	// Timeouts for startup and shutdown. Populated from Options by
	// NewServer; tests construct a *server directly to override them.
	connectTimeout    time.Duration
	shutdownTimeout   time.Duration
	readHeaderTimeout time.Duration
}

// NewServer creates a new server. Must pass in Mongo Client Options.
func NewServer(opts *Options) Server {

	router := opts.Router

	// Create router groups, skipping if the router isn't set. Start() will
	// catch a nil router and return an error rather than panicking here.
	var apiRouter, customRouter *gin.RouterGroup
	if router != nil {
		apiRouter = router.Group("/api")
		customRouter = router.Group(opts.CustomRouteName)
	}

	// Convert limit to string
	findLimit := strconv.Itoa(opts.FindLimit)

	return &server{
		mongoClientOpts:   opts.MongoClientOpts,
		router:            router,
		apiRouter:         apiRouter,
		customRouter:      customRouter,
		address:           opts.Address,
		defaultDB:         opts.DefaultDB,
		findLimit:         findLimit,
		maxLimit:          opts.FindMaxLimit,
		connectTimeout:    opts.ConnectTimeout,
		shutdownTimeout:   opts.ShutdownTimeout,
		readHeaderTimeout: opts.ReadHeaderTimeout,
	}
}

// Start new server
// This function blocks until a SIGINT/SIGTERM is received or an error
// occurs, at which point it shuts down the HTTP server and disconnects
// from MongoDB before returning.
func (s *server) Start() error {

	// Bound the initial Connect+Ping so a hung Mongo host can't block
	// startup indefinitely.
	connectCtx, cancel := context.WithTimeout(context.Background(), s.connectTimeout)
	defer cancel()

	var err error

	// Create MongoDB Connection
	s.mongoClient, err = mongo.Connect(connectCtx, s.mongoClientOpts)
	if err != nil {
		return err
	}
	defer func() {
		disconnectCtx, disconnectCancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer disconnectCancel()
		if disconnectErr := s.mongoClient.Disconnect(disconnectCtx); disconnectErr != nil {
			log.Printf("Error while disconnecting from MongoDB: %s\n", disconnectErr.Error())
		}
	}()

	// Test the connection
	err = s.mongoClient.Ping(connectCtx, nil)
	if err != nil {
		return err
	}

	// Ensure router isn't nil
	if s.router == nil {
		return fmt.Errorf("gin router is not set")
	}

	// Set routes
	s.createRoutes()

	httpServer := &http.Server{
		Addr:              s.address,
		Handler:           s.router,
		ReadHeaderTimeout: s.readHeaderTimeout,
	}

	// Listen for SIGINT/SIGTERM so a normal `docker stop`/`kubectl delete
	// pod` triggers the graceful shutdown below instead of killing the
	// process before the deferred Mongo disconnect above can run.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErrCh := make(chan error, 1)
	go func() {
		if serveErr := httpServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			serveErrCh <- serveErr
			return
		}
		serveErrCh <- nil
	}()

	// Block until either a shutdown signal arrives or the listener itself
	// fails (e.g. address already in use); a listener failure has nothing
	// left to gracefully shut down, so it's returned immediately.
	select {
	case <-ctx.Done():
	case err = <-serveErrCh:
		return err
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer shutdownCancel()

	return httpServer.Shutdown(shutdownCtx)
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
	s.apiRouter.POST("/collections/:name/count", s.collectionCount)
	s.apiRouter.POST("/collections/:name/aggregate", s.collectionAggregate)
}

// Add custom middleware in the /api router group.
// This allows custom additions like logging, auth, etc
func (s *server) SetAPIMiddleware(middleware ...gin.HandlerFunc) {
	s.apiRouter.Use(middleware...)
}

// Add custom middleware in the /custom router group.
// This allows custom additions like logging, auth, etc
func (s *server) SetCustomMiddleware(middleware ...gin.HandlerFunc) {
	s.customRouter.Use(middleware...)
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
		writeError(c, http.StatusInternalServerError, fmt.Sprintf("Error getting databases names: %s", err.Error()))
		return
	}

	res := bson.M{
		"Databases": dbNames,
	}

	c.JSON(http.StatusOK, res)
}

// writeError writes a {"error": "<message>"} JSON envelope with the given
// status code. All handler failure paths use this instead of ctx.String so
// error responses are consistently JSON, matching the JSON success responses.
func writeError(ctx *gin.Context, status int, message string) {
	ctx.JSON(status, bson.M{"error": message})
}

// resolveDB determines the target database for a /api/collections/... route:
// Options.DefaultDB if set, otherwise the "database" query param. If neither
// is available it writes the 400 response itself and returns ok=false, so
// callers can just `return` when ok is false.
func (s *server) resolveDB(ctx *gin.Context) (dbName string, ok bool) {
	if s.defaultDB != "" {
		return s.defaultDB, true
	}

	dbName, ok = ctx.GetQuery("database")
	if !ok {
		writeError(ctx, http.StatusBadRequest, "Database name was not passed, one is needed")
		return "", false
	}
	return dbName, true
}

// Route to get all collection names for the queried database
// /api/collections?database=app
func (s *server) getCollections(c *gin.Context) {

	dbName, ok := s.resolveDB(c)
	if !ok {
		return
	}

	collNames, err := s.mongoClient.Database(dbName).ListCollectionNames(c.Request.Context(), bson.M{})
	if err != nil {
		writeError(c, http.StatusInternalServerError, fmt.Sprintf("Error getting collection names: %s", err.Error()))
		return
	}

	res := bson.M{
		"Collections": collNames,
	}

	c.JSON(http.StatusOK, res)
}

// Runs a find on the collection. /collections/:name/find
// Valid URL parameter are 'database' and 'limit'
// Request body should have the find filter
//
//	ex) Request Body: {"UserName": "Jon"}
func (s *server) collectionFind(ctx *gin.Context) {

	dbName, ok := s.resolveDB(ctx)
	if !ok {
		return
	}

	// Get collection name, return error if one isn't passed
	collName := ctx.Param("name")
	if collName == "" {
		writeError(ctx, http.StatusBadRequest, "Collection name was not passed")
		return
	}

	// Get limit, if none was passed default to default value. Track whether the
	// caller actually passed one so it can be distinguished from the server default below.
	limitParam, limitPassed := ctx.GetQuery("limit")
	limitString := limitParam
	if !limitPassed {
		limitString = s.findLimit
	}
	limit, err := strconv.Atoi(limitString)
	if err != nil {
		writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Limit is not an int: %s", err.Error()))
		return
	}

	// If max limit is set and the limit exceeds it: reject an explicit caller-supplied
	// limit, but silently clamp the server-side default so FindMaxLimit < FindLimit
	// doesn't break every request that omits "limit".
	if s.maxLimit != 0 && limit > s.maxLimit {
		if limitPassed {
			writeError(ctx, http.StatusBadRequest, "Passed limit is greater than max limit set by server")
			return
		}
		limit = s.maxLimit
	}

	// Get filter from request body
	var filter bson.M
	err = ctx.ShouldBindJSON(&filter)
	if err != nil {
		writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Error reading body request: %s", err.Error()))
		return
	}

	opts := options.Find()
	opts.SetLimit(int64(limit))
	opts.SetAllowDiskUse(true)

	// Run find
	cursor, err := s.mongoClient.Database(dbName).Collection(collName).Find(ctx.Request.Context(), filter, opts)
	if err != nil {
		writeError(ctx, http.StatusInternalServerError, fmt.Sprintf("Error running find: %s", err.Error()))
		return
	}

	// Decode results
	var res []map[string]any
	err = cursor.All(ctx.Request.Context(), &res)
	if err != nil {
		writeError(ctx, http.StatusInternalServerError, fmt.Sprintf("Error decoding results: %s", err.Error()))
		return
	}

	ctx.JSON(http.StatusOK, res)
}

// Runs a count on the collection. /collections/:name/count
// Valid URL parameter is 'database'
// Request body should have the count filter
//
//	ex) Request Body: {"UserName": "Jon"}
func (s *server) collectionCount(ctx *gin.Context) {

	dbName, ok := s.resolveDB(ctx)
	if !ok {
		return
	}

	// Get collection name, return error if one isn't passed
	collName := ctx.Param("name")
	if collName == "" {
		writeError(ctx, http.StatusBadRequest, "Collection name was not passed")
		return
	}

	// Get filter from request body
	var filter bson.M
	err := ctx.ShouldBindJSON(&filter)
	if err != nil {
		writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Error reading body request: %s", err.Error()))
		return
	}

	// Run find
	count, err := s.mongoClient.Database(dbName).Collection(collName).CountDocuments(ctx.Request.Context(), filter)
	if err != nil {
		writeError(ctx, http.StatusInternalServerError, fmt.Sprintf("Error running find: %s", err.Error()))
		return
	}

	ctx.JSON(http.StatusOK, bson.M{"Count": count})
}

// Runs an aggregate on the collection
// /collections/:name/aggregate
// Valid URL parameter are 'database' and 'limit'
// Request body should contain the aggregate command, the "aggregate" key is matched case-insensitively
//
//	ex) Request Body: {"Aggregate": [{"$match": { "UserName": "Jon" }}]
func (s *server) collectionAggregate(ctx *gin.Context) {

	dbName, ok := s.resolveDB(ctx)
	if !ok {
		return
	}

	// Get collection name, return error if one isn't passed
	collName := ctx.Param("name")
	if collName == "" {
		writeError(ctx, http.StatusBadRequest, "Collection name was not passed")
		return
	}

	// Get limit, if none was passed default to default value. Track whether the
	// caller actually passed one so it can be distinguished from the server default below.
	limitParam, limitPassed := ctx.GetQuery("limit")
	limitString := limitParam
	if !limitPassed {
		limitString = s.findLimit
	}
	limit, err := strconv.Atoi(limitString)
	if err != nil {
		writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Limit is not an int: %s", err.Error()))
		return
	}

	// If max limit is set and the limit exceeds it: reject an explicit caller-supplied
	// limit, but silently clamp the server-side default so FindMaxLimit < FindLimit
	// doesn't break every request that omits "limit".
	if s.maxLimit != 0 && limit > s.maxLimit {
		if limitPassed {
			writeError(ctx, http.StatusBadRequest, "Passed limit is greater than max limit set by server")
			return
		}
		limit = s.maxLimit
	}

	// Get request body
	var reqBody map[string]any
	err = ctx.ShouldBind(&reqBody)
	if err != nil {
		writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Error reading body request: %s", err.Error()))
		return
	}

	// Get pipeline, matching the "aggregate" key case-insensitively so callers
	// sending lowercase JSON (the common case) aren't silently ignored. If no
	// recognizable key is present, an empty pipeline will be used.
	var pipeLine []any
	for key, rawPipeline := range reqBody {
		if !strings.EqualFold(key, "Aggregate") {
			continue
		}
		var ok bool
		pipeLine, ok = rawPipeline.([]any)
		if !ok {
			writeError(ctx, http.StatusBadRequest, "Aggregate field must be an array")
			return
		}
		break
	}

	// Cap the result count by appending a trailing $limit stage. This is
	// applied unconditionally, even if the caller's pipeline ends in a
	// $group or $sort, since a final $limit bounds the result count
	// regardless of what produced it.
	pipeLine = append(pipeLine, bson.M{"$limit": limit})

	opts := options.Aggregate()
	opts.SetAllowDiskUse(true)

	cursor, err := s.mongoClient.Database(dbName).Collection(collName).Aggregate(ctx.Request.Context(), pipeLine, opts)
	if err != nil {
		writeError(ctx, http.StatusInternalServerError, fmt.Sprintf("Error running aggregate: %s", err.Error()))
		return
	}

	// Decode results
	var res []map[string]any
	err = cursor.All(ctx.Request.Context(), &res)
	if err != nil {
		writeError(ctx, http.StatusInternalServerError, fmt.Sprintf("Error decoding results: %s", err.Error()))
		return
	}

	ctx.JSON(http.StatusOK, res)
}

// Add custom GET request, path will be under the /custom route group
func (s *server) AddCustomGET(relativePath string, handlers ...gin.HandlerFunc) {
	s.customRouter.GET(relativePath, handlers...)
}

// Add custom POST request, path will be under the /custom route group
func (s *server) AddCustomPOST(relativePath string, handlers ...gin.HandlerFunc) {
	s.customRouter.POST(relativePath, handlers...)
}

// Returns server mongo client.
// This can be used along side AddCustomGET() and AddCustomPost() to make custom routes that use the db.
func (s *server) GetMongoClient() *mongo.Client {
	return s.mongoClient
}
