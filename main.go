package mim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/ONSdigital/dp-mongodb-in-memory/download"
	"github.com/ONSdigital/dp-mongodb-in-memory/monitor"
	"github.com/ONSdigital/log.go/v2/log"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// max time allowed for mongo to start
const timeout = 5 * time.Second

// Server represents a running MongoDB server.
type Server struct {
	cmd        *exec.Cmd
	watcherCmd *exec.Cmd
	dbDir      string
	port       int
	replSet    string
}

// Start runs a MongoDB server at a given version using a random free port
// and returns the Server.
func Start(ctx context.Context, version string) (*Server, error) {
	server := &Server{replSet: "rs0"}

	binPath, err := getOrDownloadBinPath(ctx, version)
	if err != nil {
		log.Fatal(ctx, "Could not find mongodb", err)
		return nil, err
	}

	// Create a db dir. Even the ephemeralForTest engine needs a dbpath.
	server.dbDir, err = os.MkdirTemp("", "")
	if err != nil {
		log.Fatal(ctx, "Error creating data directory", err)
		return nil, err
	}

	log.Info(ctx, "Starting mongod server", log.Data{"binPath": binPath, "dbDir": server.dbDir})

	// Find a free port for the server - unfortunately the initial idea of allowing the mongo server to chose its own port
	// (by setting a port of 0 in the server commandline) will not work as it interferes with the later replica set initialisation
	server.cmd = exec.Command(binPath, "--replSet", "rs0", "--dbpath", server.dbDir, "--port", getFreeMongoPort())

	startupErrCh := make(chan error)
	startupPortCh := make(chan int)
	stdHandler := stdHandler(ctx, startupPortCh, startupErrCh)
	server.cmd.Stdout = stdHandler
	server.cmd.Stderr = stdHandler

	// Run the server
	err = server.cmd.Start()
	if err != nil {
		log.Fatal(ctx, "Could not start mongodb", err)
		server.Stop(ctx)
		return nil, err
	}

	log.Info(ctx, "Starting watcher")
	// Start a watcher: the watcher is a subprocess that ensures if this process
	// dies, the mongo server will be killed (and not reparented under init)
	server.watcherCmd, err = monitor.Run(os.Getpid(), server.cmd.Process.Pid)
	if err != nil {
		log.Error(ctx, "Could not start watcher", err)
		server.Stop(ctx)
		return nil, err
	}

	delay := time.NewTimer(timeout)
	select {
	case server.port = <-startupPortCh:
	case err := <-startupErrCh:
		// Ensure timer is stopped and its resources are freed
		if !delay.Stop() {
			// if the timer has been stopped then read from the channel
			<-delay.C
		}
		server.Stop(ctx)
		return nil, err
	case <-delay.C:
		server.Stop(ctx)
		return nil, errors.New("timed out waiting for mongod to start")
	}

	// Initialise the server as a replica set
	c, err := mongo.Connect(ctx, options.Client().ApplyURI(server.URI()+"/admin?directConnection=true"))
	if err != nil {
		return nil, err
	}
	replSetConfig := fmt.Sprintf(`{"_id": "rs0", "members": [{"_id": 0, "host": "localhost:%d"}]}`, server.Port())
	res := c.Database("admin").RunCommand(ctx, bson.D{{"replSetInitiate", replSetConfig}})
	if err = res.Err(); err != nil {
		return nil, err
	}

	log.Info(ctx, fmt.Sprintf("mongod started up with port number %d, and replicata set name %s", server.Port(), server.ReplicaSet()))

	return server, nil
}

// Stop kills the mongo server.
func (s *Server) Stop(ctx context.Context) {
	if s.cmd != nil {
		err := s.cmd.Process.Kill()
		if err != nil {
			log.Error(ctx, "Error stopping mongod process", err, log.Data{"pid": s.cmd.Process.Pid})
		}
	}

	if s.watcherCmd != nil {
		err := s.watcherCmd.Process.Kill()
		if err != nil {
			log.Error(ctx, "error stopping watcher process", err, log.Data{"pid": s.watcherCmd.Process.Pid})
		}
	}

	err := os.RemoveAll(s.dbDir)
	if err != nil {
		log.Error(ctx, "Error removing data directory", err, log.Data{"dir": s.dbDir})
	}
}

// Port returns the port the server is listening on.
func (s *Server) Port() int {
	return s.port
}

// URI returns a mongodb:// URI to connect to
func (s *Server) URI() string {
	return fmt.Sprintf("mongodb://localhost:%d", s.port)
}

// ReplicaSet returns the Replica Set name being used by the server (cluster of 1)
func (s *Server) ReplicaSet() string {
	return s.replSet
}

func getOrDownloadBinPath(ctx context.Context, version string) (string, error) {
	config, err := download.NewConfig(ctx, version)
	if err != nil {
		log.Error(ctx, "Failed to create config", err)
		return "", err
	}

	if err := download.GetMongoDB(ctx, *config); err != nil {
		return "", err
	}
	return config.MongoPath(), nil
}

// stdHandler handler relays messages from stdout/stderr to our logger.
// It accepts 2 channels:
// errCh will receive any error logged,
// okCh will receive the port number if mongodb started successfully
func stdHandler(ctx context.Context, okCh chan<- int, errCh chan<- error) io.Writer {
	reader, writer := io.Pipe()

	go func() {
		scanner := bufio.NewScanner(reader)

		for scanner.Scan() {
			text := scanner.Text()
			var logMessage log.Data
			err := json.Unmarshal([]byte(text), &logMessage)
			if err != nil {
				// Output the message as is if not json
				log.Info(ctx, fmt.Sprintf("[mongod] %s", text))
			} else {
				message := logMessage["msg"]
				severity := logMessage["s"]
				if severity == "E" || severity == "F" {
					// error or fatal
					errCh <- fmt.Errorf("mongod startup failed: %s", message)
				} else if severity == "I" && message == "Waiting for connections" {
					// Mongo running successfully: find port
					attr := logMessage["attr"].(map[string]interface{})
					okCh <- int(attr["port"].(float64))
				}

				log.Info(ctx, fmt.Sprintf("[mongod] %s", message), logMessage)
			}
		}

		if err := scanner.Err(); err != nil {
			log.Error(ctx, "reading mongod stdout/stderr failed: %s", err)
		}
	}()

	return writer
}

// getFreeMongoPort is simple utility to find a free port on the "localhost" interface of the host machine
// for a local mongo server to use. If any error occurs the default mongo port of "27017" is returned
func getFreeMongoPort() (port string) {
	l, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP(`127.0.0.1`)})
	if err != nil {
		return "27017"
	}
	defer func(l *net.TCPListener) {
		if err := l.Close(); err != nil {
			port = "27017"
		}
	}(l)

	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}
