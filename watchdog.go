package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"time"

	"github.com/fiam/stringutil"
)

const (
	defaultWatchdogInterval = 300
	defaultTimeout          = 60
)

type dog interface {
	check() error
}

type runDog struct {
	argv []string
}

func (d *runDog) check() error {
	cmd := exec.Command(d.argv[0], d.argv[1:]...)
	return cmd.Run()
}

func (d *runDog) String() string {
	return fmt.Sprintf("run: %s", d.argv)
}

func dialTimeout(timeout int) func(string, string) (net.Conn, error) {
	to := time.Second * time.Duration(timeout)
	return func(network, addr string) (net.Conn, error) {
		conn, err := net.DialTimeout(network, addr, to)
		if err != nil {
			return nil, err
		}
		conn.SetDeadline(time.Now().Add(to))
		return conn, nil
	}
}

func getTimeout(name string, args []string) (int, error) {
	var timeout int
	if len(args) > 2 {
		var err error
		timeout, err = strconv.Atoi(args[2])
		if err != nil {
			return 0, fmt.Errorf("%s watchdog second argument must be integer, not %s", name, args[2])
		}
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return timeout, nil
}

type connectDog struct {
	proto   string
	addr    string
	timeout int
}

func (d *connectDog) connectProto() string {
	if d.proto == "" {
		return "tcp"
	}
	return d.proto
}

func (d *connectDog) check() error {
	proto := d.proto
	if proto == "" {
		proto = "tcp"
	}
	conn, err := dialTimeout(d.timeout)(d.connectProto(), d.addr)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func (d *connectDog) String() string {
	return fmt.Sprintf("connect to: %s (%s)", d.addr, d.connectProto())
}

type getDog struct {
	url     string
	timeout int
}

func (d *getDog) check() error {
	req, err := http.NewRequest("GET", d.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("%s watchdog", AppName))
	client := &http.Client{}
	client.Transport = &http.Transport{
		Dial: dialTimeout(d.timeout),
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("non-200 error code %d", resp.StatusCode)
	}
	return nil
}

func (d *getDog) String() string {
	return fmt.Sprintf("GET: %s", d.url)
}

type Watchdog struct {
	service *Service
	dog     dog
	stop    chan bool
	stopped chan bool
}

func (w *Watchdog) Start(s *Service, interval int) error {
	w.service = s
	w.stop = make(chan bool, 1)
	w.stopped = make(chan bool, 1)
	ticker := time.NewTicker(time.Second * time.Duration(interval))
	go func() {
		for {
		stopWatchdog:
			select {
			case <-w.stop:
				ticker.Stop()
				w.stopped <- true
				break stopWatchdog
			case <-ticker.C:
				s.infof("running watchdog %s", w.dog)
				if err := w.Check(); err != nil {
					s.errorf("watchdog returned an error: %s", err)
					if err := s.stopService(); err == nil {
						s.startService()
					}
				} else {
					s.infof("watchdog finished successfully")
				}
			}
		}
	}()
	return nil
}

func (w *Watchdog) Check() error {
	return w.dog.check()
}

func (w *Watchdog) Stop() {
	if w.stop != nil {
		w.stop <- true
		<-w.stopped
		w.stop = nil
		w.stopped = nil
	}
}

func (w *Watchdog) Parse(input string) error {
	if input == "" {
		return nil
	}
	args, err := stringutil.SplitFields(input, " ")
	if err != nil {
		return err
	}
	if len(args) > 0 {
		switch args[0] {
		case "run":
			if len(args) == 1 {
				return fmt.Errorf("run watchdog requires at least one argument")
			}
			w.dog = &runDog{args[1:]}
		case "connect":
			if len(args) != 2 && len(args) != 3 {
				return fmt.Errorf("connect watchdog requires one or two arguments, %d given", len(args))
			}
			u, err := url.Parse(args[1])
			if err != nil {
				return fmt.Errorf("invalid connect URL %q: %s", args[1], err)
			}
			if u.Scheme != "tcp" && u.Scheme != "udp" {
				return fmt.Errorf("invalid connect URL scheme %q - must be tcp or udp", u.Scheme)
			}
			if _, _, err := net.SplitHostPort(u.Host); err != nil {
				return fmt.Errorf("address %q must specifiy a host and a port", u.Host)
			}
			timeout, err := getTimeout("connect", args)
			if err != nil {
				return err
			}
			w.dog = &connectDog{u.Scheme, u.Host, timeout}
		case "get":
			if len(args) != 2 && len(args) != 3 {
				return fmt.Errorf("get watchdog requires two or three arguments, %d given", len(args))
			}
			u, err := url.Parse(args[1])
			if err != nil {
				return fmt.Errorf("invalid GET URL %q: %s", args[1], err)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("invalid GET URL scheme %q - must be http or https", u.Scheme)
			}
			timeout, err := getTimeout("get", args)
			if err != nil {
				return err
			}
			w.dog = &getDog{args[1], timeout}
		}
	}
	if w.dog == nil {
		return fmt.Errorf("invalid watchdog %q - available watchdogs are run, connect and get", input)
	}
	return nil
}
