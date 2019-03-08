// Package etcd provides the etcd backend plugin.
package etcd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/etcd/msg"
	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/coredns/coredns/plugin/proxy"
	"github.com/coredns/coredns/request"

	"github.com/coredns/coredns/plugin/pkg/upstream"
	etcdc "github.com/coreos/etcd/client"
	"github.com/miekg/dns"
	"golang.org/x/net/context"
)

// Etcd is a plugin talks to an etcd cluster.
type Etcd struct {
	Next       plugin.Handler
	Fall       fall.F
	Zones      []string
	PathPrefix string
	Upstream   upstream.Upstream // Proxy for looking up names during the resolution process
	Client     etcdc.KeysAPI
	Ctx        context.Context
	Stubmap    *map[string]proxy.Proxy // list of proxies for stub resolving.

	endpoints []string // Stored here as well, to aid in testing.

	WildcardBound int8 // Calculate the boundary of WildcardDNS
}

// Services implements the ServiceBackend interface.
func (e *Etcd) Services(state request.Request, exact bool, opt plugin.Options) (services []msg.Service, err error) {
	services, err = e.Records(state, exact)
	if err != nil {
		return
	}

	services = msg.Group(services)
	return
}

// Reverse implements the ServiceBackend interface.
func (e *Etcd) Reverse(state request.Request, exact bool, opt plugin.Options) (services []msg.Service, err error) {
	return e.Services(state, exact, opt)
}

// Lookup implements the ServiceBackend interface.
func (e *Etcd) Lookup(state request.Request, name string, typ uint16) (*dns.Msg, error) {
	return e.Upstream.Lookup(state, name, typ)
}

// IsNameError implements the ServiceBackend interface.
func (e *Etcd) IsNameError(err error) bool {
	if ee, ok := err.(etcdc.Error); ok && ee.Code == etcdc.ErrorCodeKeyNotFound {
		return true
	}
	return false
}

// Records looks up records in etcd. If exact is true, it will lookup just this
// name. This is used when find matches when completing SRV lookups for instance.
func (e *Etcd) Records(state request.Request, exact bool) ([]msg.Service, error) {
	name := state.Name()
	qType := state.QType()
	subPath := ""
	star := false
	hasSubDomain := false

	// No need to lookup the domain which is like zone name
	// for example:
	//  name: lb.rancher.cloud.
	//  zones: [lb.rancher.cloud]
	// "lb.rancher.cloud." shold not lookup any keys in etcd
	for _, zone := range e.Zones {
		if strings.HasPrefix(name, zone) {
			return nil, nil
		}
	}

	temp := dns.SplitDomainName(name)
	start := int8(len(temp)) - e.WildcardBound
	if e.WildcardBound > 0 && qType != dns.TypeTXT && start > 0 {
		subPath = e.hasSubDomains(name)
		if subPath != "" && !strings.Contains(name, "*") {
			hasSubDomain = true
		} else {
			name = fmt.Sprintf("*.%s", strings.Join(temp[start:], "."))
		}
	}

	if qType == dns.TypeTXT && strings.Contains(name, "_acme-challenge") {
		// Only for ACME DNS challenge (dns-01) txt record, such as _acme-challenge.xx.xx
		// need add _txt level after the root level
		// for example:
		//   name: _acme-challenge.a1.lb.rancher.cloud.
		//   reverse: cloud.rancher.lb.a1._acme-challenge._txt
		//   path: /skydns/_txt/_acme-challenge/a1/lb/rancher/cloud
		temp := dns.SplitDomainName(name)
		for index, value := range temp {
			if index == 0 {
				name = value + "._txt"
				continue
			}
			name = value + "." + name
		}
	}

	var path string
	var segments []string

	if !hasSubDomain {
		path, star = msg.PathWithWildcard(name, e.PathPrefix)
		segments = strings.Split(msg.Path(name, e.PathPrefix), "/")
	} else {
		// Converts the current sub-domain to the path of the etcd
		path = msg.PathSubDomain(subPath, e.WildcardBound, dns.SplitDomainName(name))
		segments = strings.Split(path, "/")
	}

	r, err := e.get(path, true)
	if err != nil {
		// If subdomain doesn't match any of the record, return the record of the root domain
		if hasSubDomain && etcdc.IsKeyNotFound(err) {
			temp := dns.SplitDomainName(name)
			upper := temp[(int8(len(temp)) - e.WildcardBound):]
			name = fmt.Sprintf("*.%s", strings.Join(upper, "."))
			path, star = msg.PathWithWildcard(name, e.PathPrefix)
			segments = strings.Split(msg.Path(name, e.PathPrefix), "/")
			resp, err := e.get(path, true)
			if err != nil {
				return nil, err
			}
			return e.loopNodes(resp.Node.Nodes, segments, star, nil)
		}
		return nil, err
	}
	switch {
	case exact && r.Node.Dir:
		return nil, nil
	case r.Node.Dir:
		return e.loopNodes(r.Node.Nodes, segments, star, nil)
	default:
		return e.loopNodes([]*etcdc.Node{r.Node}, segments, false, nil)
	}
}

// get is a wrapper for client.Get
func (e *Etcd) get(path string, recursive bool) (*etcdc.Response, error) {
	ctx, cancel := context.WithTimeout(e.Ctx, etcdTimeout)
	defer cancel()
	r, err := e.Client.Get(ctx, path, &etcdc.GetOptions{Sort: false, Recursive: recursive})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// skydns/local/skydns/east/staging/web
// skydns/local/skydns/west/production/web
//
// skydns/local/skydns/*/*/web
// skydns/local/skydns/*/web

// loopNodes recursively loops through the nodes and returns all the values. The nodes' keyname
// will be match against any wildcards when star is true.
func (e *Etcd) loopNodes(ns []*etcdc.Node, nameParts []string, star bool, bx map[msg.Service]bool) (sx []msg.Service, err error) {
	if bx == nil {
		bx = make(map[msg.Service]bool)
	}
Nodes:
	for _, n := range ns {
		if n.Dir {
			nodes, err := e.loopNodes(n.Nodes, nameParts, star, bx)
			if err != nil {
				return nil, err
			}
			sx = append(sx, nodes...)
			continue
		}
		if star {
			keyParts := strings.Split(n.Key, "/")
			for i, n := range nameParts {
				if i > len(keyParts)-1 {
					// name is longer than key
					continue Nodes
				}
				if n == "*" || n == "any" {
					continue
				}
				if keyParts[i] != n {
					continue Nodes
				}
			}
		}
		serv := new(msg.Service)
		if err := json.Unmarshal([]byte(n.Value), serv); err != nil {
			return nil, fmt.Errorf("%s: %s", n.Key, err.Error())
		}
		b := msg.Service{Host: serv.Host, Port: serv.Port, Priority: serv.Priority, Weight: serv.Weight, Text: serv.Text, Key: n.Key}
		if _, ok := bx[b]; ok {
			continue
		}
		bx[b] = true

		serv.Key = n.Key
		serv.TTL = e.TTL(n, serv)
		if serv.Priority == 0 {
			serv.Priority = priority
		}
		sx = append(sx, *serv)
	}
	return sx, nil
}

func (e *Etcd) hasSubDomains(name string) string {
	p := msg.RootPathSubDomain(name, e.WildcardBound, e.PathPrefix)
	resp, err := e.get(p, true)
	if err != nil {
		return ""
	}
	if len(resp.Node.Nodes) > 0 {
		return p
	}
	return ""
}

// TTL returns the smaller of the etcd TTL and the service's
// TTL. If neither of these are set (have a zero value), a default is used.
func (e *Etcd) TTL(node *etcdc.Node, serv *msg.Service) uint32 {
	etcdTTL := uint32(node.TTL)

	if etcdTTL == 0 && serv.TTL == 0 {
		return ttl
	}
	if etcdTTL == 0 {
		return serv.TTL
	}
	if serv.TTL == 0 {
		return etcdTTL
	}
	if etcdTTL < serv.TTL {
		return etcdTTL
	}
	return serv.TTL
}

const (
	priority    = 10  // default priority when nothing is set
	ttl         = 300 // default ttl when nothing is set
	etcdTimeout = 5 * time.Second
)
