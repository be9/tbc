package client

import (
	"crypto/tls"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// DialGrpc creates a new gRPC client connected to host. tlsKey and tlsCert must be either both empty or non-empty.
func DialGrpc(host, tlsCert, tlsKey string) (*grpc.ClientConn, error) {
	var creds credentials.TransportCredentials

	if tlsCert != "" || tlsKey != "" {
		if tlsCert == "" || tlsKey == "" {
			return nil, fmt.Errorf("only one of tlsCert and tlsKey was provided")
		}
		cert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
		})
	} else {
		creds = insecure.NewCredentials()
	}
	return grpc.NewClient(host, grpc.WithTransportCredentials(creds))
}
