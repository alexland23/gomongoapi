package gomongoapi

import (
	"testing"
)

func TestOptions_SetCustomRouteName(t *testing.T) {

	type args struct {
		customRouteName string
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			name: "Valid",
			args: args{
				customRouteName: "/custom",
			},
			wantErr: false,
		},
		{
			name: "Invalid - Root path",
			args: args{
				customRouteName: "/",
			},
			wantErr: true,
		},
		{
			name: "Invalid - Api path",
			args: args{
				customRouteName: "/api",
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := ServerOptions()
			if err := o.SetCustomRouteName(tt.args.customRouteName); (err != nil) != tt.wantErr {
				t.Errorf("Options.SetCustomRouteName() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
