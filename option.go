package gomongoapi

import (
	"errors"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	ErrInvalidCustomRouteName = errors.New("invalid custom route name")
)

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
}

// Returns server options with default values
func ServerOptions() *Options {
	return &Options{
		Router:          gin.Default(),
		Address:         ":8080",
		CustomRouteName: "custom",
		MongoClientOpts: options.Client(),
		FindLimit:       1000,
		FindMaxLimit:    0,
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

// SetAddress sets the server address.
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
