// Lucas Kirsche
// Copyright 2020 Oz Tiram <oz.tiram@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package netbox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/metrics"
	"github.com/coredns/coredns/plugin/pkg/fall"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

// Define log to be a logger with the plugin name in it. This way we can just use log.Info and
// friends to log.
var log = clog.NewWithPlugin("netbox")

type Netbox struct {
	Url       string
	Token     string
	Next      plugin.Handler
	TTL       time.Duration
	Fall      fall.F
	Zones     []string
	UsePlugin bool
	Client    *http.Client
}

// constants to match IP address family used by NetBox
const (
	familyIP4 = 4
	familyIP6 = 6
)

// ServeDNS implements the plugin.Handler interface
func (n *Netbox) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	var (
		err error
	)

	state := request.Request{W: w, Req: r}

	// only handle zones we are configured to respond for
	zone := plugin.Zones(n.Zones).Matches(state.Name())
	if zone == "" {
		return plugin.NextOrFailure(n.Name(), n.Next, ctx, w, r)
	}

	// Export metric with the server label set to the current
	// server handling the request.
	requestCount.WithLabelValues(metrics.WithServer(ctx)).Inc()

	var answers []dns.RR

	if n.UsePlugin {
		answers, err = n.queryDNSPlugin(zone, state)
	} else {
		answers, err = n.queryNative(state)
	}

	if err != nil {
		// always fallthrough if configured
		if n.Fall.Through(state.Name()) {
			return plugin.NextOrFailure(n.Name(), n.Next, ctx, w, r)
		}

		// otherwise return SERVFAIL here without fallthrough
		return dnserror(dns.RcodeServerFailure, state, err)
	}

	if len(answers) == 0 {
		if n.Fall.Through(state.Name()) {
			return plugin.NextOrFailure(n.Name(), n.Next, ctx, w, r)
		} else {
			return dnserror(dns.RcodeNameError, state, nil)
		}
	}

	// create DNS response
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	m.Answer = answers

	// send response back to client
	_ = w.WriteMsg(m)

	// signal response sent back to client
	return dns.RcodeSuccess, nil
}

// Name implements the Handler interface.
func (n *Netbox) Name() string { return "netbox" }

func (n *Netbox) queryNative(state request.Request) ([]dns.RR, error) {
	var (
		ips     []net.IP
		domains []string
		answers []dns.RR
		err     error
	)
	qname := state.Name()
	// check record type here and bail out if not A, AAAA or PTR
	switch state.QType() {
	case dns.TypeA:
		ips, err = n.query(strings.TrimRight(qname, "."), familyIP4)
		answers = a(qname, uint32(n.TTL.Seconds()), ips)
	case dns.TypeAAAA:
		ips, err = n.query(strings.TrimRight(qname, "."), familyIP6)
		answers = aaaa(qname, uint32(n.TTL.Seconds()), ips)
	case dns.TypePTR:
		domains, err = n.queryreverse(qname)
		answers = ptr(qname, uint32(n.TTL.Seconds()), domains)
	default:
		return nil, fmt.Errorf("request type not implemented")
	}
	return answers, err
}

func (n *Netbox) queryDNSPlugin(zone string, state request.Request) ([]dns.RR, error) {
	var (
		records []DNSRecord
		zones   []DNSZone
		answers []dns.RR = make([]dns.RR, 0)
		err     error
	)
	qname := state.Name()
	qtype := state.QType()

	if qtype == dns.TypeSOA {
		zones, err = n.queryZone(zone)
	} else {
		querySet, OK := DNSQueryReverseMap[qtype]
		if !OK {
			return nil, fmt.Errorf("request type not implemented")
		}
		records, err = n.queryRecord(zone, qname, querySet)
	}

	for _, record := range records {
		// try to resolve CNAME record if question was A or AAAA
		if record.Type == DNSRecordTypeCNAME && (qtype == dns.TypeA || qtype == dns.TypeAAAA) {
			if resolvedRecs, err := n.queryRecord(zone, record.AbsoluteValue, DNSQueryReverseMap[qtype]); err == nil {
				records = append(records, resolvedRecs...)
			}
		}
		answers = append(answers, record.RR())
	}
	for _, zone := range zones {
		answers = append(answers, zone.RR())
	}
	return answers, err
}

// a takes a slice of net.IPs and returns a slice of A RRs.
func a(zone string, ttl uint32, ips []net.IP) []dns.RR {
	answers := make([]dns.RR, len(ips))
	for i, ip := range ips {
		r := new(dns.A)
		r.Hdr = dns.RR_Header{Name: zone, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}
		r.A = ip
		answers[i] = r
	}
	return answers
}

// aaaa takes a slice of net.IPs and returns a slice of AAAA RRs.
func aaaa(zone string, ttl uint32, ips []net.IP) []dns.RR {
	answers := make([]dns.RR, len(ips))
	for i, ip := range ips {
		r := new(dns.AAAA)
		r.Hdr = dns.RR_Header{Name: zone, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl}
		r.AAAA = ip
		answers[i] = r
	}
	return answers
}

// ptr takes a slice of strings and returns a slice of PTR RRs.
func ptr(zone string, ttl uint32, domains []string) []dns.RR {

	answers := make([]dns.RR, len(domains))
	for i, domain := range domains {
		r := new(dns.PTR)
		r.Hdr = dns.RR_Header{Name: zone, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl}
		r.Ptr = domain
		answers[i] = r
	}

	return answers
}

// dnserror writes a DNS error response back to the client. Based on plugin.BackendError
func dnserror(rcode int, state request.Request, err error) (int, error) {
	m := new(dns.Msg)
	m.SetRcode(state.Req, rcode)
	m.Authoritative = true

	// send response
	_ = state.W.WriteMsg(m)

	// return success as the rcode to signal we have written to the client.
	return dns.RcodeSuccess, err
}
