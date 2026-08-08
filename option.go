package gomongoapi

import (
	"errors"
	"time"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ErrInvalidCustomRouteName is returned by SetCustomRouteName when the given name is reserved.
var ErrInvalidCustomRouteName = errors.New("invalid custom route name")

// Options contains options to configure the mongo api server
type Options struct {
	// Gin engine that server will use, gin.Default() is the default value.
	Router *gin.Engine

	// Server address that the gin router with use. Default is :8080
	Address string

	// Optional field to set custom route group name which will be used if user adds custom routes. Default is 'custom'.
	CustomRouteName string

	// Mongo Client options. Default is an empty set of options.
	MongoClientOpts *options.ClientOptions

	// Default value of number of records find will return if one is not passed in url.
	FindLimit int

	// An upper limit of the number of records that find can return. Default is 0 which means no limit.
	FindMaxLimit int

	// Optional field if user wants to set a default database to use. If none is set then all databases will be queryable.
	DefaultDB string

	// Upper bound on the initial Mongo Connect+Ping during Start(), so a hung Mongo host can't
	// block startup indefinitely. Default is 10 seconds.
	ConnectTimeout time.Duration

	// Upper bound on each of the graceful HTTP shutdown and the subsequent Mongo disconnect when
	// Start() receives SIGINT/SIGTERM. Default is 10 seconds.
	ShutdownTimeout time.Duration

	// Upper bound on how long the HTTP server waits to read a request's headers, guarding against
	// slow-header (Slowloris) attacks. Default is 5 seconds.
	ReadHeaderTimeout time.Duration

	// Upper bound on the Mongo ping issued by the /api/health readiness route, so a hung Mongo
	// host can't make the health check itself hang. Default is 5 seconds.
	HealthCheckTimeout time.Duration
}

// ServerOptions returns server options with default values.
func ServerOptions() *Options {
	return &Options{
		Router:             gin.Default(),
		Address:            ":8080",
		CustomRouteName:    "custom",
		MongoClientOpts:    options.Client(),
		FindLimit:          1000,
		FindMaxLimit:       0,
		ConnectTimeout:     defaultConnectTimeout,
		ShutdownTimeout:    defaultShutdownTimeout,
		ReadHeaderTimeout:  defaultReadHeaderTimeout,
		HealthCheckTimeout: defaultHealthCheckTimeout,
	}
}

// SetRouter sets the gin engine that will be used.
func (o *Options) SetRouter(router *gin.Engine) {
	o.Router = router
}

// SetAddress sets the server address.
func (o *Options) SetAddress(address string) {
	o.Address = address
}

// SetCustomRouteName sets custom route name.
func (o *Options) SetCustomRouteName(customRouteName string) error {
	// Ensure custom route is not root or api
	if customRouteName == `/` || customRouteName == `/api` {
		return ErrInvalidCustomRouteName
	}

	o.CustomRouteName = customRouteName
	return nil
}

// SetMongoClientOpts sets the mongo client options used to connect to MongoDB.
func (o *Options) SetMongoClientOpts(mongoClientOpts *options.ClientOptions) {
	o.MongoClientOpts = mongoClientOpts
}

// SetDefaultDB sets the default db to be used in the collection routes.
// This value is option as a db name can be passed to the routes.
func (o *Options) SetDefaultDB(defaultDB string) {
	o.DefaultDB = defaultDB
}

// SetFindLimit sets the default limit when running find.
func (o *Options) SetFindLimit(findLimit int) {
	o.FindLimit = findLimit
}

// SetFindMaxLimit sets the upper limit for find results.
func (o *Options) SetFindMaxLimit(findMaxLimit int) {
	o.FindMaxLimit = findMaxLimit
}

// SetConnectTimeout sets the upper bound on the initial Mongo Connect+Ping during Start().
func (o *Options) SetConnectTimeout(connectTimeout time.Duration) {
	o.ConnectTimeout = connectTimeout
}

// SetShutdownTimeout sets the upper bound on each of the graceful HTTP shutdown and the
// subsequent Mongo disconnect when Start() receives SIGINT/SIGTERM.
func (o *Options) SetShutdownTimeout(shutdownTimeout time.Duration) {
	o.ShutdownTimeout = shutdownTimeout
}

// SetReadHeaderTimeout sets the upper bound on how long the HTTP server waits to read a
// request's headers.
func (o *Options) SetReadHeaderTimeout(readHeaderTimeout time.Duration) {
	o.ReadHeaderTimeout = readHeaderTimeout
}

// SetHealthCheckTimeout sets the upper bound on the Mongo ping issued by the /api/health
// readiness route.
func (o *Options) SetHealthCheckTimeout(healthCheckTimeout time.Duration) {
	o.HealthCheckTimeout = healthCheckTimeout
}
