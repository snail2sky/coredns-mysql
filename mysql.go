package coredns_mysql_extend

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/coredns/coredns/plugin"

	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	_ "github.com/go-sql-driver/mysql"
	"github.com/miekg/dns"
)

var logger = clog.NewWithPlugin(pluginName)

func (m *Mysql) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	var records []record
	state := request.Request{W: w, Req: r}
	answers := make([]dns.RR, 0)
	rrStrings := make([]string, 0)

	// Get domain name
	qName := state.Name()
	qType := state.Type()
	degradeRecord := record{fqdn: qName, qType: qType}

	logger.Debugf("FQDN %s, DNS query type %s", qName, qType)

	// Query zone cache
	zoneID, host, zone, err := m.getDomainInfo(qName)
	logger.Debugf("ZoneID %d, host %s, zone %s", zoneID, host, zone)

	// Zone not exist, maybe db error cause no zone, goto degrade entrypoint
	if err != nil {
		logger.Error(err)
		goto DegradeEntrypoint
	}

	// Query DB, full match
	records, err = m.getRecords(zoneID, host, zone, qType)
	logger.Debugf("zone id %d, host %s, zone %s, type %s, records %#v", zoneID, host, zone, qType, records)
	if err != nil {
		logger.Errorf("Failed to get records for domain %s from database: %s", qName, err)
		goto DegradeEntrypoint
	}

	// Try query CNAME type of record
	if len(records) == zero {
		cnameRecords, err := m.getRecords(zoneID, host, zone, cnameQtype)
		logger.Debugf("zone id %d, host %s, zone %s, type %s, records %#v", zoneID, host, zone, cnameQtype, records)
		if err != nil {
			logger.Errorf("Failed to get records for domain %s from database: %s", qName, err)
			goto DegradeEntrypoint
		}
		for _, cnameRecord := range cnameRecords {
			cnameZoneID, cnameHost, cnameZone, err := m.getDomainInfo(cnameRecord.data)
			logger.Debugf("ZoneID %d, host %s, zone %s", cnameZoneID, cnameHost, cnameZone)

			if err != nil {
				logger.Error(err)
				goto DegradeEntrypoint
			}

			rrString := fmt.Sprintf("%s %d IN %s %s", qName, cnameRecord.ttl, cnameRecord.qType, cnameRecord.data)
			rrStrings = append(rrStrings, rrString)
			rr, err := dns.NewRR(rrString)
			if err != nil {
				logger.Errorf("Failed to create DNS record: %s", err)
				continue
			}
			answers = append(answers, rr)

			cname2Records, err := m.getRecords(cnameZoneID, cnameHost, cnameZone, qType)
			logger.Debugf("zone id %d, host %s, zone %s, qType %s, records %#v", cnameZoneID, cnameHost, cnameZone, qType, records)

			if err != nil {
				logger.Errorf("Failed to get domain %s from database: %s", cnameHost+zoneSeparator+cnameZone, err)
				goto DegradeEntrypoint
			}

			for _, cname2Record := range cname2Records {
				rrString := fmt.Sprintf("%s %d IN %s %s", cname2Record.name+zoneSeparator+cname2Record.zoneName, cname2Record.ttl, cname2Record.qType, cname2Record.data)
				rrStrings = append(rrStrings, rrString)
				rr, err := dns.NewRR(rrString)
				if err != nil {
					logger.Errorf("Failed to create DNS record: %s", err)
					continue
				}
				answers = append(answers, rr)
			}
		}
	}

	// Process records
	for _, record := range records {
		rrString := fmt.Sprintf("%s %d IN %s %s", record.name, record.ttl, record.qType, record.data)
		rr, err := dns.NewRR(rrString)
		rrStrings = append(rrStrings, rrString)
		if err != nil {
			logger.Errorf("Failed to create DNS record: %s", err)
			continue
		}
		answers = append(answers, rr)
	}

	// Handle wildcard domains
	if len(answers) == zero && strings.Count(qName, zoneSeparator) > 1 {
		baseZone := m.getBaseZone(qName)
		domainID, ok := m.getZoneID(baseZone)
		wildcardName := wildcard + zoneSeparator + baseZone
		if !ok {
			logger.Errorf("Failed to get zone %s from database: %s", qName, err)
			goto DegradeEntrypoint
		}
		records, err := m.getRecords(domainID, wildcard, zone, qType)
		if err != nil {
			logger.Errorf("Failed to get records for domain %s from database: %s", wildcardName, err)
			goto DegradeEntrypoint
		}

		for _, record := range records {
			rrString := fmt.Sprintf("%s %d IN %s %s", wildcardName, record.ttl, record.qType, record.data)
			rr, err := dns.NewRR(rrString)
			rrStrings = append(rrStrings, rrString)
			if err != nil {
				logger.Errorf("Failed to create DNS record: %s", err)
				continue
			}
			answers = append(answers, rr)
		}
	}

	// Return result
	if len(answers) > 0 {
		msg := MakeMessage(r, answers)
		w.WriteMsg(msg)
		// DegradeEntrypoint cache
		dnsRecordInfo := dnsRecordInfo{rrStrings: rrStrings, response: answers}
		if cacheDnsRecordInfo, ok := m.degradeCache[degradeRecord]; !ok || !reflect.DeepEqual(cacheDnsRecordInfo, dnsRecordInfo) {
			m.degradeCache[degradeRecord] = dnsRecordInfo
			logger.Debugf("Add degrade record %#v, dnsRecordInfo %#v", degradeRecord, dnsRecordInfo)
			return dns.RcodeSuccess, nil
		}

	}

	// DegradeEntrypoint
DegradeEntrypoint:
	if answers, ok := m.degradeQuery(degradeRecord); ok {
		msg := MakeMessage(r, answers)
		w.WriteMsg(msg)
		logger.Debugf("Add degrade record %#v", degradeRecord)

		return dns.RcodeSuccess, nil
	}
	logger.Debug("Call next plugin")
	return plugin.NextOrFailure(m.Name(), m.Next, ctx, w, r)
}

// func (m *Mysql) Debug() {
// 	logger.Debugf("[DEBUG] MySQL plugin configuration: %+v", m)
// }

// func (m *Mysql) Metrics() []plugin.Metric {
// 	return nil
// }
