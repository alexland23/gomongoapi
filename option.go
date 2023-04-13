package mongoapi

import (
	"github.com/gin-gonic/gin"
)

// Options contains options to configure the mongo api server
type Option struct {
	// Gin engine that server will use, gin.Default() is the default value.
	Router *gin.Engine
	// Server address that the gin router with use. Default is :8080
	Address string
	// Default value of number of records find will return if one is not passed in url.
	FindLimit int
	// An upper limit of the number of records that find can return. Default is 0 which means no limit.
	FindMaxLimit int
	// Optional field if user wants to set a default database to use. If none is set then all databases will be queriable.
	DefaultDB string
}

// Returns server options with default values
func ServerOptions() *Option {
	return &Option{
		Router:       gin.Default(),
		Address:      ":8080",
		FindLimit:    1000,
		FindMaxLimit: 0,
	}
}

// SetRouter sets the gin engine that will be used.
func (o *Option) SetRouter(router *gin.Engine) {
	o.Router = router
}

// SetAddress sets the server address
func (o *Option) SetAddress(address string) {
	o.Address = address
}

// SetDefaultDB sets the default db to be used in the collection routes.
// This value is option as a db name can be passed to the routes.
func (o *Option) SetDefaultDB(defaultDB string) {
	o.DefaultDB = defaultDB
}

// SetFindLimit sets the default limit when running find.
func (o *Option) SetFindLimit(findLimit int) {
	o.FindLimit = findLimit
}

func (o *Option) SetFindMaxLimit(findMaxLimit int) {
	o.FindMaxLimit = findMaxLimit
}
