//
// Copyright 2015 Gregory Trubetskoy. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package timeriver

import (
	"bufio"
	"fmt"
	pickle "github.com/hydrogen18/stalecucumber"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

type trService interface {
	File() *os.File
	Start(*os.File) error
	Stop()
}

type serviceMap map[string]trService
type trServiceManager struct {
	t        *trTransceiver
	services serviceMap
}

func newServiceManager(t *trTransceiver) *trServiceManager {
	return &trServiceManager{t: t,
		services: serviceMap{
			"gt":  &graphiteTextServiceManager{t: t},
			"gu":  &graphiteUdpTextServiceManager{t: t},
			"gp":  &graphitePickleServiceManager{t: t},
			"www": &wwwServer{t: t},
		},
	}
}

func (r *trServiceManager) run(gracefulProtos string) error {

	// TODO If a listen-spec changes in the config and a graceful
	// restart is issued, the new config will not take effect as the
	// open file is reused.

	if gracefulProtos == "" {
		for _, service := range r.services {
			if err := service.Start(nil); err != nil {
				return err
			}
		}
	} else {

		protos := strings.Split(gracefulProtos, ",")

		for n, p := range protos {
			f := os.NewFile(uintptr(n+3), "")
			if r.services[p] != nil {
				if err := r.services[p].Start(f); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *trServiceManager) listenerFilesAndProtocols() ([]*os.File, string) {

	files := []*os.File{}
	protos := []string{}

	for name, service := range r.services {
		files = append(files, service.File())
		protos = append(protos, name)
	}
	return files, strings.Join(protos, ",")
}

func (r *trServiceManager) closeListeners() {
	for _, service := range r.services {
		service.Stop()
	}
}

// ---

type wwwServer struct {
	t        *trTransceiver
	listener *gracefulListener
}

func (g *wwwServer) File() *os.File {
	if g.listener != nil {
		return g.listener.File()
	}
	return nil
}

func (g *wwwServer) Stop() {
	if g.listener != nil {
		g.listener.Close()
	}
}

func (g *wwwServer) Start(file *os.File) error {
	var (
		gl  net.Listener
		err error
	)

	if config.HttpListenSpec != "" {
		if file != nil {
			gl, err = net.FileListener(file)
		} else {
			gl, err = net.Listen("tcp", config.HttpListenSpec)
		}
	} else {
		fmt.Printf("Not starting HTTP server because http-listen-spec is blank.\n")
		log.Printf("Not starting HTTP server because http-listen-spec is blank.")
		return nil
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error starting HTTP protocol: %v\n", err)
		return fmt.Errorf("Error starting HTTP protocol: %v", err)
	}

	g.listener = newGracefulListener(gl)

	fmt.Printf("HTTP protocol Listening on %s\n", config.HttpListenSpec)

	go httpServer(config.HttpListenSpec, g.listener, g.t)

	return nil
}

// ---

type graphitePickleServiceManager struct {
	t        *trTransceiver
	listener *gracefulListener
}

func (g *graphitePickleServiceManager) File() *os.File {
	if g.listener != nil {
		return g.listener.File()
	}
	return nil
}

func (g *graphitePickleServiceManager) Stop() {
	if g.listener != nil {
		g.listener.Close()
	}
}

func (g *graphitePickleServiceManager) Start(file *os.File) error {
	var (
		gl  net.Listener
		err error
	)

	if config.GraphitePickleListenSpec != "" {
		if file != nil {
			gl, err = net.FileListener(file)
		} else {
			gl, err = net.Listen("tcp", config.GraphitePickleListenSpec)
		}
	} else {
		log.Printf("Not starting Graphite Pickle Protocol because graphite-pickle-listen-spec is blank.")
		return nil
	}

	if err != nil {
		return fmt.Errorf("Error starting Graphite Pickle Protocol serviceManager: %v", err)
	}

	g.listener = newGracefulListener(gl)

	fmt.Printf("Graphite Pickle protocol Listening on %s\n", config.GraphitePickleListenSpec)

	go g.graphitePickleServer()

	return nil
}

func (g *graphitePickleServiceManager) graphitePickleServer() error {

	var tempDelay time.Duration
	for {
		conn, err := g.listener.Accept()

		// This code comes from the golang http lib, it attempts to
		// retry accepting a connection when too many files are open
		// under heavy load.
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				log.Printf("Accept error: %v; retrying in %v", err, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return err
		}
		tempDelay = 0

		go handleGraphitePickleProtocol(g.t, conn, 10)
	}
}

func handleGraphitePickleProtocol(t *trTransceiver, conn net.Conn, timeout int) {

	defer conn.Close() // decrements tcpWg

	if timeout != 0 {
		conn.SetDeadline(time.Now().Add(time.Duration(timeout) * time.Second))
	}

	// We use the Scanner, becase it has a MaxScanTokenSize of 64K

	var (
		name                 string
		tstamp               int64
		int_value            int64
		value                float64
		err                  error
		item                 interface{}
		items, itemSlice, dp []interface{}
	)

	items, err = pickle.ListOrTuple(pickle.Unpickle(conn))
	if err == nil {
		for _, item = range items {
			itemSlice, err = pickle.ListOrTuple(item, err)
			if len(itemSlice) == 2 {
				name, err = pickle.String(itemSlice[0], err)
				dp, err = pickle.ListOrTuple(itemSlice[1], err)
				if len(dp) == 2 {
					tstamp, err = pickle.Int(dp[0], err)
					if value, err = pickle.Float(dp[1], err); err != nil {
						if _, ok := err.(pickle.WrongTypeError); ok {
							if int_value, err = pickle.Int(dp[1], nil); err == nil {
								value = float64(int_value)
							}
						}
					}
					t.queueDataPoint(name, time.Unix(tstamp, 0), value)
				} else {
					err = fmt.Errorf("dp wrong length: %d", len(dp))
					break
				}
			} else {
				err = fmt.Errorf("item wrong length: %d", len(itemSlice))
				break
			}
		}
	}

	if timeout != 0 {
		conn.SetDeadline(time.Now().Add(time.Duration(timeout) * time.Second))
	}

	if err != nil {
		log.Println("handleGraphitePickleProtocol(): Error reading:", err.Error())
	}
}

// --

type graphiteUdpTextServiceManager struct {
	t    *trTransceiver
	conn net.Conn
}

func (g *graphiteUdpTextServiceManager) Stop() {
	if g.conn != nil {
		g.conn.Close()
	}
}

func (g *graphiteUdpTextServiceManager) File() *os.File {
	if g.conn != nil {
		f, _ := g.conn.(*net.UDPConn).File()
		return f
	}
	return nil
}

func (g *graphiteUdpTextServiceManager) Start(file *os.File) error {
	var (
		err     error
		udpAddr *net.UDPAddr
	)

	if config.GraphiteUdpListenSpec != "" {
		if file != nil {
			g.conn, err = net.FileConn(file)
		} else {
			udpAddr, err = net.ResolveUDPAddr("udp", config.GraphiteUdpListenSpec)
			if err == nil {
				g.conn, err = net.ListenUDP("udp", udpAddr)
			}
		}
	} else {
		log.Printf("Not starting Graphite UDP protocol because graphite-udp-listen-spec is blank.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("Error starting Graphite UDP Text Protocol serviceManager: %v", err)
	}

	fmt.Printf("Graphite UDP protocol Listening on %s\n", config.GraphiteTextListenSpec)

	// for UDP timeout must be 0
	go handleGraphiteTextProtocol(g.t, g.conn, 0)

	return nil
}

// ---

type graphiteTextServiceManager struct {
	t        *trTransceiver
	listener *gracefulListener
}

func (g *graphiteTextServiceManager) File() *os.File {
	if g.listener != nil {
		return g.listener.File()
	}
	return nil
}

func (g *graphiteTextServiceManager) Stop() {
	if g.listener != nil {
		g.listener.Close()
	}
}

func (g *graphiteTextServiceManager) Start(file *os.File) error {
	var (
		gl  net.Listener
		err error
	)

	if config.GraphiteTextListenSpec != "" {
		if file != nil {
			gl, err = net.FileListener(file)
		} else {
			gl, err = net.Listen("tcp", config.GraphiteTextListenSpec)
		}
	} else {
		log.Printf("Not starting Graphite Text protocol because graphite-test-listen-spec is blank")
		return nil
	}

	if err != nil {
		return fmt.Errorf("Error starting Graphite Text Protocol serviceManager: %v", err)
	}

	g.listener = newGracefulListener(gl)

	fmt.Println("Graphite text protocol Listening on " + config.GraphiteTextListenSpec)

	go g.graphiteTextServer()

	return nil
}

func (g *graphiteTextServiceManager) graphiteTextServer() error {

	var tempDelay time.Duration
	for {
		conn, err := g.listener.Accept()

		if err != nil {
			// see http://golang.org/src/net/http/server.go?s=51504:51550#L1729
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				log.Printf("newGracefulListener(): Accept error: %v; retrying in %v", err, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return err
		}
		tempDelay = 0

		go handleGraphiteTextProtocol(g.t, conn, 10)
	}
}

// Handles incoming requests for both TCP and UDP
func handleGraphiteTextProtocol(t *trTransceiver, conn net.Conn, timeout int) {

	defer conn.Close() // decrements tcpWg

	if timeout != 0 {
		conn.SetDeadline(time.Now().Add(time.Duration(timeout) * time.Second))
	}

	// We use the Scanner, becase it has a MaxScanTokenSize of 64K

	var (
		name   string
		tstamp int64
		value  float64
	)

	connbuf := bufio.NewScanner(conn)

	for connbuf.Scan() {
		str := connbuf.Text()
		n, err := fmt.Sscanf(str, "%s %f %d", &name, &value, &tstamp)

		if n != 3 || err != nil {
			log.Printf("handleGraphiteTextProtocol(): error %v scanning input: %q", err, str)
		}
		t.queueDataPoint(name, time.Unix(tstamp, 0), value)

		if timeout != 0 {
			conn.SetDeadline(time.Now().Add(time.Duration(timeout) * time.Second))
		}
	}

	if err := connbuf.Err(); err != nil {
		log.Println("handleGraphiteTextProtocol(): Error reading: %v", err)
	}
}