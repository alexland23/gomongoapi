package gomongoapi

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestServerOptionsDefaults(t *testing.T) {
	opts := ServerOptions()

	require.NotNil(t, opts)
	assert.NotNil(t, opts.Router)
	assert.Equal(t, ":8080", opts.Address)
	assert.Equal(t, "custom", opts.CustomRouteName)
	assert.NotNil(t, opts.MongoClientOpts)
	assert.Equal(t, 1000, opts.FindLimit)
	assert.Equal(t, 0, opts.FindMaxLimit)
	assert.Equal(t, "", opts.DefaultDB)
}

func TestSetRouter(t *testing.T) {
	opts := ServerOptions()
	router := gin.New()

	opts.SetRouter(router)

	assert.Same(t, router, opts.Router)
}

func TestSetAddress(t *testing.T) {
	opts := ServerOptions()

	opts.SetAddress(":9090")

	assert.Equal(t, ":9090", opts.Address)
}

func TestSetCustomRouteName(t *testing.T) {
	tests := []struct {
		name      string
		routeName string
		wantErr   error
	}{
		{name: "valid route name", routeName: "myroutes", wantErr: nil},
		{name: "rejects root", routeName: "/", wantErr: ErrInvalidCustomRouteName},
		{name: "rejects api", routeName: "/api", wantErr: ErrInvalidCustomRouteName},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := ServerOptions()

			err := opts.SetCustomRouteName(tt.routeName)

			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				// Value should remain unchanged from default when rejected
				assert.Equal(t, "custom", opts.CustomRouteName)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.routeName, opts.CustomRouteName)
		})
	}
}

func TestSetMongoClientOpts(t *testing.T) {
	opts := ServerOptions()
	mongoOpts := options.Client().ApplyURI("mongodb://localhost:27017")

	opts.SetMongoClientOpts(mongoOpts)

	assert.Same(t, mongoOpts, opts.MongoClientOpts)
}

func TestSetDefaultDB(t *testing.T) {
	opts := ServerOptions()

	opts.SetDefaultDB("app")

	assert.Equal(t, "app", opts.DefaultDB)
}

func TestSetFindLimit(t *testing.T) {
	opts := ServerOptions()

	opts.SetFindLimit(500)

	assert.Equal(t, 500, opts.FindLimit)
}

func TestSetFindMaxLimit(t *testing.T) {
	opts := ServerOptions()

	opts.SetFindMaxLimit(2000)

	assert.Equal(t, 2000, opts.FindMaxLimit)
}
