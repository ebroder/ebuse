package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"sync"

	"github.com/abligh/gonbdserver/nbd"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ebs"
)

var flags = flag.NewFlagSet("", flag.ExitOnError)
var region string
var socket string

func usage() {
	fmt.Fprintf(flags.Output(), "usage: %s [flags] snap-12345678\n", os.Args[0])
	flags.PrintDefaults()
	os.Exit(2)
}

func init() {
	defaultRegion, ok := os.LookupEnv("AWS_REGION")
	if !ok {
		defaultRegion = "us-east-1"
	}

	defaultSocketDir, ok := os.LookupEnv("XDG_RUNTIME_DIR")
	if !ok {
		defaultSocketDir = "/tmp"
	}
	defaultSocket := path.Join(defaultSocketDir, "nbd.sock")

	flags.StringVar(&region, "region", defaultRegion, "AWS region of snapshot")
	flags.StringVar(&socket, "socket", defaultSocket, "path to listen on")

	flags.Usage = usage
}

func main() {
	flags.Parse(os.Args)

	args := flags.Args()
	if len(args) != 2 {
		usage()
	}
	snapshot := args[1]

	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(region),
	}))
	client := ebs.New(sess)

	nbd.RegisterBackend("ebs", func(ctx context.Context, e *nbd.ExportConfig) (nbd.Backend, error) {
		return NewSnapshotBackend(ctx, client, snapshot)
	})

	os.Remove(socket)

	ctx, cancelFunc := context.WithCancel(context.Background())
	var sessionWaitGroup sync.WaitGroup
	defer func() {
		cancelFunc()
		sessionWaitGroup.Wait()
	}()

	go func() {
		nbd.StartServer(ctx, ctx, &sessionWaitGroup, log.New(os.Stderr, "", log.LstdFlags), nbd.ServerConfig{
			Protocol:      "unix",
			Address:       socket,
			DefaultExport: "ebs",
			Exports: []nbd.ExportConfig{
				{
					Name:     "ebs",
					Driver:   "ebs",
					ReadOnly: true,
				},
			},
		})
	}()

	<-ctx.Done()
}
