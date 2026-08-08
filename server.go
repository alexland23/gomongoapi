/*
Package gomongoapi is a pure go client that allows for easy creation of a server that creates routes to query a MongoDB.
The intent of these routes is to be used alongside either the JSON API or Infinity plugin with Grafana to allow for
MongoDB dashboards within Grafana.

Package is using gin for the server and can be heavily customized as a custom gin engine can be set in the options.

Available default routes:

	+----------------------------------+-----------+-------+------------------------------------------------------------------------------------------------------+
	| Path                             | HTTP Verb | Body  | Result                                                                                               |
	+----------------------------------+-----------+-------+------------------------------------------------------------------------------------------------------+
	| /                                |    GET    | Empty | Always 200, test connection (liveness).                                                              |
	| /api/health                      |    GET    | Empty | Pings MongoDB, 200 if reachable, 503 otherwise (readiness).                                          |
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
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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

// defaultHealthCheckTimeout bounds how long the /api/health route waits on
// its Mongo ping, so a hung Mongo host can't make the health check itself
// hang.
const defaultHealthCheckTimeout = 5 * time.Second

// defaultMaxBodyBytes bounds the size of request bodies accepted by the /api
// group, so a large find/count/aggregate body can't be used to pressure
// server memory. 1 MiB comfortably fits any realistic filter or pipeline.
const defaultMaxBodyBytes = 1 << 20

// Server interface for mongo api server
type Server interface {

	// Start new server
	// This function blocks until a SIGINT/SIGTERM is received or an error
	// occurs, at which point it shuts down the HTTP server and disconnects
	// from MongoDB before returning.
	Start() error

	// Add custom middleware in the /api router group.
	// This allows custom additions like logging, auth, etc.
	//
	// Must be called before Start(): gin composes a route's handler chain at
	// registration time, and routes are registered during Start(). Calling
	// this after Start() has begun (e.g. from another goroutine) will not
	// protect the already-registered routes; it logs a warning rather than
	// silently doing nothing, since this is commonly used as an auth hook.
	SetAPIMiddleware(middleware ...gin.HandlerFunc)

	// Add custom middleware in the /custom router group.
	// This allows custom additions like logging, auth, etc.
	//
	// Must be called before Start(): gin composes a route's handler chain at
	// registration time, and routes are registered during Start(). Calling
	// this after Start() has begun (e.g. from another goroutine) will not
	// protect the already-registered routes; it logs a warning rather than
	// silently doing nothing, since this is commonly used as an auth hook.
	SetCustomMiddleware(middleware ...gin.HandlerFunc)

	// SetAPIKey installs a built-in check on the /api group: requests must send the given
	// key in the X-API-Key header or get 401 Unauthorized. It's a convenience wrapper
	// around SetAPIMiddleware for callers who don't already have their own gateway auth in
	// front of the server; the same "must be called before Start()" ordering constraint
	// applies (see SetAPIMiddleware). For anything beyond a single shared secret (per-caller
	// keys, JWT, OAuth), use SetAPIMiddleware directly instead.
	SetAPIKey(apiKey string)

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
	mongoClientOpts      *options.ClientOptions
	mongoClient          *mongo.Client
	defaultDB            string
	findLimit            string
	maxLimit             int
	allowUnsafeOperators bool

	// Timeouts for startup and shutdown. Populated from Options by
	// NewServer; tests construct a *server directly to override them.
	connectTimeout     time.Duration
	shutdownTimeout    time.Duration
	readHeaderTimeout  time.Duration
	healthCheckTimeout time.Duration

	// Guards routesRegistered, which SetAPIMiddleware/SetCustomMiddleware
	// check to warn about late registration (see createRoutes).
	mu               sync.Mutex
	routesRegistered bool
}

// NewServer creates a new server. Must pass in Mongo Client Options.
func NewServer(opts *Options) Server {

	router := opts.Router

	// Create router groups, skipping if the router isn't set. Start() will
	// catch a nil router and return an error rather than panicking here.
	var apiRouter, customRouter *gin.RouterGroup
	if router != nil {
		apiRouter = router.Group("/api")
		// Registered before any caller-added middleware (SetAPIMiddleware
		// runs after NewServer returns) and before createRoutes() attaches
		// routes in Start(), so it applies to every /api request regardless
		// of what's added later.
		apiRouter.Use(maxBodyBytesMiddleware(opts.MaxBodyBytes))
		customRouter = router.Group(opts.CustomRouteName)
	}

	// Convert limit to string
	findLimit := strconv.Itoa(opts.FindLimit)

	return &server{
		mongoClientOpts:      opts.MongoClientOpts,
		router:               router,
		apiRouter:            apiRouter,
		customRouter:         customRouter,
		address:              opts.Address,
		defaultDB:            opts.DefaultDB,
		findLimit:            findLimit,
		maxLimit:             opts.FindMaxLimit,
		connectTimeout:       opts.ConnectTimeout,
		shutdownTimeout:      opts.ShutdownTimeout,
		readHeaderTimeout:    opts.ReadHeaderTimeout,
		healthCheckTimeout:   opts.HealthCheckTimeout,
		allowUnsafeOperators: opts.AllowUnsafeOperators,
	}
}

// maxBodyBytesMiddleware wraps the request body in an http.MaxBytesReader so
// reads beyond maxBodyBytes fail with an *http.MaxBytesError, guarding
// against large find/count/aggregate bodies pressuring server memory. A
// maxBodyBytes of 0 disables the limit.
func maxBodyBytesMiddleware(maxBodyBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBodyBytes > 0 {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		}
		c.Next()
	}
}

// writeActions are Mongo privilege actions that indicate the connected user can write
// data. Used by warnIfWritableCredentials to flag when MongoClientOpts isn't scoped to a
// read-only user, per the README Security section's "Use a read-only Mongo user" guidance.
var writeActions = map[string]bool{
	"insert": true,
	"update": true,
	"remove": true,
}

// connectionStatusPrivilege is one entry of authInfo.authenticatedUserPrivileges in the
// connectionStatus admin command's response (with showPrivileges:true).
type connectionStatusPrivilege struct {
	Actions []string `bson:"actions"`
}

// connectionStatusResult decodes the subset of the connectionStatus command's response
// needed to inspect the authenticated user's privileges.
type connectionStatusResult struct {
	AuthInfo struct {
		AuthenticatedUserPrivileges []connectionStatusPrivilege `bson:"authenticatedUserPrivileges"`
	} `bson:"authInfo"`
}

// hasWriteAction reports whether any privilege in privileges grants a write action, and if
// so, which one (for the log message).
func hasWriteAction(privileges []connectionStatusPrivilege) (action string, found bool) {
	for _, priv := range privileges {
		for _, action := range priv.Actions {
			if writeActions[action] {
				return action, true
			}
		}
	}
	return "", false
}

// warnIfWritableCredentials runs the connectionStatus admin command and logs a warning if
// the authenticated user holds any write privilege on the connected Mongo cluster. This is
// best-effort and advisory only: it never fails Start(), and a command failure (e.g. no
// auth configured, or a custom role that disallows connectionStatus) is silently ignored
// rather than surfaced — a read-only Mongo user remains the load-bearing control regardless
// of whether this check can run, see the README Security section.
func (s *server) warnIfWritableCredentials(ctx context.Context) {
	cmd := bson.D{{Key: "connectionStatus", Value: 1}, {Key: "showPrivileges", Value: 1}}

	var result connectionStatusResult
	if err := s.mongoClient.Database("admin").RunCommand(ctx, cmd).Decode(&result); err != nil {
		return
	}

	if action, found := hasWriteAction(result.AuthInfo.AuthenticatedUserPrivileges); found {
		log.Printf("gomongoapi: configured MongoClientOpts credentials have write privileges (e.g. %q); a read-only Mongo user is strongly recommended, see README Security section", action)
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

	s.warnIfWritableCredentials(connectCtx)

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

	// Mark routes as registered so any later SetAPIMiddleware/
	// SetCustomMiddleware call can warn that it won't affect these routes:
	// gin composes a route's handler chain at registration time, which
	// happens here.
	s.mu.Lock()
	s.routesRegistered = true
	s.mu.Unlock()

	// Test connection, always return ok
	s.router.GET("/", func(ctx *gin.Context) {
		ctx.Status(http.StatusOK)
	})

	// Create api group
	s.apiRouter.GET("/health", s.health)
	s.apiRouter.GET("/databases", s.getDatabases)
	s.apiRouter.GET("/collections", s.getCollections)
	s.apiRouter.POST("/collections/:name/find", s.collectionFind)
	s.apiRouter.POST("/collections/:name/count", s.collectionCount)
	s.apiRouter.POST("/collections/:name/aggregate", s.collectionAggregate)
}

// Add custom middleware in the /api router group.
// This allows custom additions like logging, auth, etc. Must be called
// before Start(); see the Server interface doc for why.
func (s *server) SetAPIMiddleware(middleware ...gin.HandlerFunc) {
	s.warnIfRoutesRegistered("SetAPIMiddleware")
	s.apiRouter.Use(middleware...)
}

// Add custom middleware in the /custom router group.
// This allows custom additions like logging, auth, etc. Must be called
// before Start(); see the Server interface doc for why.
func (s *server) SetCustomMiddleware(middleware ...gin.HandlerFunc) {
	s.warnIfRoutesRegistered("SetCustomMiddleware")
	s.customRouter.Use(middleware...)
}

// SetAPIKey installs a built-in check on the /api group: requests must send the given key
// in the X-API-Key header or get 401 Unauthorized. It's a thin wrapper around
// SetAPIMiddleware, so the same "must be called before Start()" ordering constraint
// applies.
func (s *server) SetAPIKey(apiKey string) {
	s.SetAPIMiddleware(apiKeyMiddleware(apiKey))
}

// apiKeyMiddleware rejects requests whose X-API-Key header doesn't match apiKey, using a
// constant-time comparison so response timing can't be used to guess the key.
func apiKeyMiddleware(apiKey string) gin.HandlerFunc {
	key := []byte(apiKey)
	return func(c *gin.Context) {
		if subtle.ConstantTimeCompare([]byte(c.GetHeader("X-API-Key")), key) != 1 {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

// warnIfRoutesRegistered logs loudly if routes have already been registered
// (i.e. Start() has called createRoutes()), since gin composes a route's
// handler chain at registration time: middleware added after that point has
// no effect on the routes already registered, which would otherwise be a
// silent no-op for what's commonly an auth hook.
func (s *server) warnIfRoutesRegistered(method string) {
	s.mu.Lock()
	registered := s.routesRegistered
	s.mu.Unlock()

	if registered {
		log.Printf("gomongoapi: %s called after Start(); it will not apply to already-registered routes", method)
	}
}

// Route to check readiness: pings MongoDB and returns 503 if it's
// unreachable, unlike "/" which always returns 200 regardless of the Mongo
// connection's state. The ping is bounded by healthCheckTimeout so a hung
// Mongo host can't make this route itself hang.
func (s *server) health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), s.healthCheckTimeout)
	defer cancel()

	if err := s.mongoClient.Ping(ctx, nil); err != nil {
		c.JSON(http.StatusServiceUnavailable, bson.M{
			"status": "error",
			"error":  fmt.Sprintf("Error pinging MongoDB: %s", err.Error()),
		})
		return
	}

	c.JSON(http.StatusOK, bson.M{"status": "ok"})
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

// bindJSONBody decodes the request body as JSON into v, always via the JSON
// binder regardless of the request's Content-Type. An empty body (io.EOF) is
// treated as success, leaving v at its zero value, so callers can omit the
// body entirely rather than being forced to send "{}".
func bindJSONBody(ctx *gin.Context, v any) error {
	err := ctx.ShouldBindJSON(v)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// writeBindError writes the error response for a bindJSONBody failure: 413
// if the body exceeded the server's MaxBodyBytes limit (enforced by
// maxBodyBytesMiddleware), 400 for any other malformed-body error.
func writeBindError(ctx *gin.Context, err error) {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		writeError(ctx, http.StatusRequestEntityTooLarge, fmt.Sprintf("Error reading body request: %s", err.Error()))
		return
	}
	writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Error reading body request: %s", err.Error()))
}

// parseSortParam parses the "sort" url query param (a JSON object, e.g.
// {"createdAt":-1,"name":1}) into an ordered sort specification for
// options.Find().SetSort. It's decoded manually via encoding/json's token
// stream rather than into a map, because Go map iteration order is
// randomized and, for a multi-field sort, field order is significant.
func parseSortParam(raw string) (bson.D, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New(`sort must be a JSON object, e.g. {"field":1}`)
	}

	var sort bson.D
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("sort keys must be strings")
		}

		var direction int
		if err := dec.Decode(&direction); err != nil || (direction != 1 && direction != -1) {
			return nil, fmt.Errorf("sort value for %q must be 1 or -1", key)
		}
		sort = append(sort, bson.E{Key: key, Value: direction})
	}
	return sort, nil
}

// bannedOperators are Mongo operators rejected by default in find/count filters and
// aggregate pipelines: $where/$function can execute arbitrary server-side JavaScript, and
// $out/$merge can write to or overwrite a collection via what looks like a query API. See
// the README Security section. Opt out via Options.AllowUnsafeOperators.
var bannedOperators = map[string]bool{
	"$where":    true,
	"$function": true,
	"$out":      true,
	"$merge":    true,
}

// findBannedOperator recursively walks a decoded JSON value (maps/slices, as produced by
// encoding/json) looking for any key in bannedOperators, so a banned operator nested inside
// $and/$or/$expr or a pipeline stage can't bypass a shallow, top-level-only check.
func findBannedOperator(v any) (op string, found bool) {
	switch val := v.(type) {
	case map[string]any:
		for k, sub := range val {
			if bannedOperators[k] {
				return k, true
			}
			if op, found = findBannedOperator(sub); found {
				return op, found
			}
		}
	case bson.M:
		return findBannedOperator(map[string]any(val))
	case []any:
		for _, sub := range val {
			if op, found = findBannedOperator(sub); found {
				return op, found
			}
		}
	}
	return "", false
}

// endsInOutOrMerge reports whether pipeline's last stage is $out or $merge, which Mongo
// requires to be the pipeline's terminal stage — collectionAggregate uses this to avoid
// appending its own trailing $limit stage after one.
func endsInOutOrMerge(pipeline []any) bool {
	if len(pipeline) == 0 {
		return false
	}
	stage, ok := pipeline[len(pipeline)-1].(map[string]any)
	if !ok {
		return false
	}
	return stage["$out"] != nil || stage["$merge"] != nil
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
// Valid URL parameters are 'database', 'limit', 'skip', and 'sort'.
// 'sort' is a JSON object url-encoded into the query string, e.g.
// ?sort={"createdAt":-1}, since a JSON sort spec can't be a plain scalar.
// Request body is the find filter, with an optional top-level "projection"
// key (matched case-insensitively) pulled out as the projection spec;
// remaining keys form the filter. This makes "projection" a reserved
// top-level filter key.
//
//	ex) Request Body: {"UserName": "Jon", "projection": {"UserName": 1}}
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

	// Get skip, defaulting to 0 (no skip) if not passed.
	var skip int
	if skipParam, skipPassed := ctx.GetQuery("skip"); skipPassed {
		skip, err = strconv.Atoi(skipParam)
		if err != nil {
			writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Skip is not an int: %s", err.Error()))
			return
		}
		if skip < 0 {
			writeError(ctx, http.StatusBadRequest, "Skip must be a non-negative int")
			return
		}
	}

	// Get sort, defaulting to no sort if not passed.
	var sort bson.D
	if sortParam, sortPassed := ctx.GetQuery("sort"); sortPassed {
		sort, err = parseSortParam(sortParam)
		if err != nil {
			writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Invalid sort parameter: %s", err.Error()))
			return
		}
	}

	// Get filter from request body. An empty body means "no filter". A
	// top-level "projection" key (matched case-insensitively) is pulled out
	// as the projection spec; the remaining keys form the filter.
	var rawBody map[string]any
	if err := bindJSONBody(ctx, &rawBody); err != nil {
		writeBindError(ctx, err)
		return
	}

	var projection bson.M
	for key, val := range rawBody {
		if !strings.EqualFold(key, "projection") {
			continue
		}
		m, ok := val.(map[string]any)
		if !ok {
			writeError(ctx, http.StatusBadRequest, "Projection field must be an object")
			return
		}
		projection = bson.M(m)
		delete(rawBody, key)
		break
	}

	filter := bson.M(rawBody)
	if filter == nil {
		filter = bson.M{}
	}

	if !s.allowUnsafeOperators {
		if op, found := findBannedOperator(map[string]any(filter)); found {
			writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Filter contains disallowed operator %q; see Options.AllowUnsafeOperators", op))
			return
		}
	}

	opts := options.Find()
	opts.SetLimit(int64(limit))
	opts.SetAllowDiskUse(true)
	opts.SetSkip(int64(skip))
	if len(sort) > 0 {
		opts.SetSort(sort)
	}
	if projection != nil {
		opts.SetProjection(projection)
	}

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

	// Get filter from request body. An empty body means "no filter".
	var filter bson.M
	if err := bindJSONBody(ctx, &filter); err != nil {
		writeBindError(ctx, err)
		return
	}
	if filter == nil {
		filter = bson.M{}
	}

	if !s.allowUnsafeOperators {
		if op, found := findBannedOperator(map[string]any(filter)); found {
			writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Filter contains disallowed operator %q; see Options.AllowUnsafeOperators", op))
			return
		}
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

	// Get request body. An empty body means "no pipeline". Bound via the JSON
	// binder specifically (not ShouldBind, which picks a binder from
	// Content-Type and would fall through to form binding, misinterpreting a
	// valid JSON body without a Content-Type: application/json header).
	var reqBody map[string]any
	if err := bindJSONBody(ctx, &reqBody); err != nil {
		writeBindError(ctx, err)
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

	if !s.allowUnsafeOperators {
		if op, found := findBannedOperator(pipeLine); found {
			writeError(ctx, http.StatusBadRequest, fmt.Sprintf("Pipeline contains disallowed stage/operator %q; see Options.AllowUnsafeOperators", op))
			return
		}
	}

	// Cap the result count by appending a trailing $limit stage. This is
	// applied unconditionally, even if the caller's pipeline ends in a
	// $group or $sort, since a final $limit bounds the result count
	// regardless of what produced it. Exception: $out/$merge (only reachable
	// with AllowUnsafeOperators) must themselves be the pipeline's terminal
	// stage, so a $limit can't be appended after one.
	if !endsInOutOrMerge(pipeLine) {
		pipeLine = append(pipeLine, bson.M{"$limit": limit})
	}

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
