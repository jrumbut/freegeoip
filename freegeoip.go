// Copyright 2013 Alexandre Fiori
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Web server of freegeoip.net

package main

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"expvar"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "code.google.com/p/gosqlite/sqlite3"
	"github.com/fiorix/go-redis/redis"
	"github.com/fiorix/go-web/httpxtra"
)

type Settings struct {
	Debug    bool     `xml:"debug,attr"`
	DebugSrv string   `xml:"debugsrv,attr"`
	XMLName  xml.Name `xml:"Server"`
	Listen   []*struct {
		Log      bool   `xml:"log,attr"`
		XHeaders bool   `xml:"xheaders,attr"`
		Addr     string `xml:"addr,attr"`
		CertFile string
		KeyFile  string
	}
	DocumentRoot string
	IPDB         struct {
		File      string `xml:",attr"`
		CacheSize string `xml:",attr"`
	}
	Limit struct {
		MaxRequests int `xml:",attr"`
		Expire      int `xml:",attr"`
	}
	Redis []string `xml:"Redis>Addr"`
}

var (
	conf        *Settings
	protoCount  = expvar.NewMap("Protocol") // HTTP or HTTPS
	outputCount = expvar.NewMap("Output")   // json, xml, csv or other
	statusCount = expvar.NewMap("Status")   // 200, 403, 404, etc
)

func main() {
	cf := flag.String("config", "freegeoip.conf", "set config file")
	flag.Parse()
	if buf, err := ioutil.ReadFile(*cf); err != nil {
		log.Fatal(err)
	} else {
		conf = &Settings{}
		if err := xml.Unmarshal(buf, conf); err != nil {
			log.Fatal(err)
		}
	}
	runtime.GOMAXPROCS(runtime.NumCPU())
	log.Printf("FreeGeoIP server starting. debug=%t", conf.Debug)
	if conf.Debug && len(conf.DebugSrv) > 0 {
		go func() {
			// server for expvar's /debug/vars only
			log.Printf("DEBUG server on %s", conf.DebugSrv)
			log.Fatal(http.ListenAndServe(conf.DebugSrv, nil))
		}()
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(conf.DocumentRoot)))
	h := LookupHandler()
	mux.HandleFunc("/csv/", h)
	mux.HandleFunc("/xml/", h)
	mux.HandleFunc("/json/", h)
	wg := new(sync.WaitGroup)
	for _, l := range conf.Listen {
		if l.Addr == "" {
			continue
		}
		wg.Add(1)
		h := httpxtra.Handler{Handler: mux, XHeaders: l.XHeaders}
		if l.Log {
			h.Logger = logger
		}
		s := http.Server{
			Addr:         l.Addr,
			Handler:      h,
			ReadTimeout:  15 * time.Second,
			WriteTimeout: 15 * time.Second,
		}
		if l.KeyFile == "" {
			log.Printf("Listening HTTP on %s "+
				"log=%t xheaders=%t",
				l.Addr, l.Log, l.XHeaders)
			go func() {
				log.Fatal(httpxtra.ListenAndServe(s))
			}()
		} else {
			log.Printf("Listening HTTPS on %s "+
				"log=%t xheaders=%t cert=%s key=%s",
				l.Addr, l.Log, l.XHeaders,
				l.CertFile, l.KeyFile)
			go func() {
				log.Fatal(s.ListenAndServeTLS(
					l.CertFile,
					l.KeyFile,
				))
			}()
		}
	}
	wg.Wait()
}

func logger(r *http.Request, created time.Time, status, bytes int) {
	//fmt.Println(httpxtra.ApacheCommonLog(r, created, status, bytes))
	var (
		s, ip string
		err   error
	)
	if r.TLS == nil {
		s = "HTTP"
	} else {
		s = "HTTPS"
	}
	if ip, _, err = net.SplitHostPort(r.RemoteAddr); err != nil {
		ip = r.RemoteAddr
	}
	log.Printf("%s %d %s %q (%s) :: %s",
		s,
		status,
		r.Method,
		r.URL.Path,
		ip,
		time.Since(created),
	)
	if conf.Debug {
		protoCount.Add(s, 1)
		statusCount.Add(fmt.Sprintf("%d", status), 1)
		switch strings.SplitN(r.URL.Path, "/", 2)[1] {
		case "json/":
			outputCount.Add("json", 1)
		case "xml/":
			outputCount.Add("xml", 1)
		case "csv/":
			outputCount.Add("csv", 1)
		default:
			outputCount.Add("other", 1)
		}
	}
}

// LookupHandler handles GET on /csv, /xml and /json.
func LookupHandler() http.HandlerFunc {
	db, err := sql.Open("sqlite3", conf.IPDB.File)
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec("PRAGMA cache_size=" + conf.IPDB.CacheSize)
	if err != nil {
		log.Fatal(err)
	}
	stmt, err := db.Prepare(query)
	if err != nil {
		log.Fatal(err)
	}
	//defer stmt.Close()
	var quota Quota
	if len(conf.Redis) == 0 {
		quota = new(MapQuota)
		quota.Setup()
		log.Printf("Using internal map to manage quota.")
	} else {
		quota = new(RedisQuota)
		quota.Setup(conf.Redis...)
		log.Printf("Using redis to manage quota: %s", conf.Redis)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case "OPTIONS":
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Access-Control-Allow-Methods", "GET")
			w.Header().Set("Access-Control-Allow-Headers", "X-Requested-With")
			w.WriteHeader(200)
			return
		default:
			w.Header().Set("Allow", "GET, OPTIONS")
			http.Error(w, http.StatusText(405), 405)
			return
		}
		// GET continues...
		var srcIP net.IP
		fmt.Println("GET continues")
		fmt.Println(net.SplitHostPort(r.RemoteAddr))
		if ip, _, err := net.SplitHostPort(r.RemoteAddr); err != nil {
			srcIP = net.ParseIP(r.RemoteAddr) // Use X-Real-IP
		} else {
			srcIP = net.ParseIP(ip)
		}
		fmt.Println("srcIP")
		fmt.Println(srcIP)
		if srcIP == nil {
			fmt.Println("srcIP is nil")
			http.Error(w, http.StatusText(400), 400)
			return
		}
		nsrcIP, err := ip2int(srcIP)
		fmt.Println("nsrcIP")
		fmt.Println(nsrcIP)
		if err != nil {
			if conf.Debug {
				log.Println(err)
			}
			//http.Error(w, http.StatusText(400), 400)
			//return
		}
		// Check quota.
		if conf.Limit.MaxRequests > 0 {
			var ok bool
			if ok, err = quota.Ok(nsrcIP); err != nil {
				if conf.Debug {
					log.Println(err) // redis error
				}
				http.Error(w, http.StatusText(503), 503)
				return
			} else if !ok {
				// Over quota, soz :(
				http.Error(w, http.StatusText(403), 403)
				return
			}
		}
		var (
			queryIP  net.IP
			nqueryIP uint32
		)
		// Parse URL (e.g. /csv/ip, /xml/)
		fmt.Println("Parse URL")
		fmt.Println(r.URL.Path)
		a := strings.SplitN(r.URL.Path, "/", 3)
		if len(a) == 3 && a[2] != "" {
			addrs, err := net.LookupHost(a[2])
			if err != nil {
				// DNS lookup failed, assume host not found.
				http.Error(w, http.StatusText(404), 404)
				return
			}
			if queryIP = net.ParseIP(addrs[0]); queryIP == nil {
				http.Error(w, http.StatusText(400), 400)
				return
			}
			nqueryIP, err = ip2int(net.ParseIP(addrs[0]))
			if err != nil {
				if conf.Debug {
					log.Println(err)
				}
				http.Error(w, http.StatusText(400), 400)
				return
			}
		} else {
			queryIP = srcIP
			nqueryIP = nsrcIP
		}
		// Query the db.
		geoip, err := lookup(stmt, queryIP, nqueryIP)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch a[1][0] {
		case 'j': // json
			resp, err := json.Marshal(geoip)
			if err != nil {
				if conf.Debug {
					log.Println("JSON error:", err.Error())
				}
				http.Error(w, http.StatusText(500), 500)
				return
			}
			if cb := r.FormValue("callback"); cb != "" {
				w.Header().Set("Content-Type", "text/javascript")
				fmt.Fprintf(w, "%s(%s);", cb, resp)
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.Write(resp)
			}
		case 'x': // xml
			w.Header().Set("Content-Type", "application/xml")
			resp, err := xml.MarshalIndent(geoip, "", " ")
			if err != nil {
				if conf.Debug {
					log.Println("XML error:", err.Error())
				}
				http.Error(w, http.StatusText(500), 500)
				return
			}
			fmt.Fprintf(w, xml.Header+"%s\n", resp)
		case 'c': // csv
			w.Header().Set("Content-Type", "application/csv")
			fmt.Fprintf(w, `"%s","%s","%s","%s","%s","%s",`+
				`"%s","%0.4f","%0.4f","%s","%s"`+"\r\n",
				geoip.Ip,
				geoip.CountryCode, geoip.CountryName,
				geoip.RegionCode, geoip.RegionName,
				geoip.CityName, geoip.ZipCode,
				geoip.Latitude, geoip.Longitude,
				geoip.MetroCode, geoip.AreaCode)
		}
	}
}

func ip2int(ip net.IP) (uint32, error) {
	var n uint32
	ipv4 := ip.To4()
	if ipv4 == nil {
		return 0, fmt.Errorf("IP %s is not IPv4", ip.String())
	}
	if err := binary.Read(
		bytes.NewBuffer(ipv4),
		binary.BigEndian,
		&n,
	); err != nil {
		return 0, fmt.Errorf("IP conversion failed: %s", err.Error())
	}
	return n, nil
}

func lookup(stmt *sql.Stmt, IP net.IP, nIP uint32) (*GeoIP, error) {
	var reserved bool
	for _, net := range reservedIPs {
		if net.Contains(IP) {
			reserved = true
			break
		}
	}
	geoip := GeoIP{Ip: IP.String()}
	if reserved {
		geoip.CountryCode = "RD"
		geoip.CountryName = "Reserved"
	} else {
		if err := stmt.QueryRow(nIP).Scan(
			&geoip.CountryCode,
			&geoip.CountryName,
			&geoip.RegionCode,
			&geoip.RegionName,
			&geoip.CityName,
			&geoip.ZipCode,
			&geoip.Latitude,
			&geoip.Longitude,
			&geoip.MetroCode,
			&geoip.AreaCode,
		); err != nil {
			return nil, err
		}
	}
	return &geoip, nil
}

type GeoIP struct {
	XMLName     xml.Name `json:"-" xml:"Response"`
	Ip          string   `json:"ip"`
	CountryCode string   `json:"country_code"`
	CountryName string   `json:"country_name"`
	RegionCode  string   `json:"region_code"`
	RegionName  string   `json:"region_name"`
	CityName    string   `json:"city" xml:"City"`
	ZipCode     string   `json:"zipcode"`
	Latitude    float32  `json:"latitude"`
	Longitude   float32  `json:"longitude"`
	MetroCode   string   `json:"metro_code"`
	AreaCode    string   `json:"areacode"`
}

// http://en.wikipedia.org/wiki/Reserved_IP_addresses
var reservedIPs = []net.IPNet{
	{net.IPv4(0, 0, 0, 0), net.IPv4Mask(255, 0, 0, 0)},
	{net.IPv4(10, 0, 0, 0), net.IPv4Mask(255, 0, 0, 0)},
	{net.IPv4(100, 64, 0, 0), net.IPv4Mask(255, 192, 0, 0)},
	{net.IPv4(127, 0, 0, 0), net.IPv4Mask(255, 0, 0, 0)},
	{net.IPv4(169, 254, 0, 0), net.IPv4Mask(255, 255, 0, 0)},
	{net.IPv4(172, 16, 0, 0), net.IPv4Mask(255, 240, 0, 0)},
	{net.IPv4(192, 0, 0, 0), net.IPv4Mask(255, 255, 255, 248)},
	{net.IPv4(192, 0, 2, 0), net.IPv4Mask(255, 255, 255, 0)},
	{net.IPv4(192, 88, 99, 0), net.IPv4Mask(255, 255, 255, 0)},
	{net.IPv4(192, 168, 0, 0), net.IPv4Mask(255, 255, 0, 0)},
	{net.IPv4(198, 18, 0, 0), net.IPv4Mask(255, 254, 0, 0)},
	{net.IPv4(198, 51, 100, 0), net.IPv4Mask(255, 255, 255, 0)},
	{net.IPv4(203, 0, 113, 0), net.IPv4Mask(255, 255, 255, 0)},
	{net.IPv4(224, 0, 0, 0), net.IPv4Mask(240, 0, 0, 0)},
	{net.IPv4(240, 0, 0, 0), net.IPv4Mask(240, 0, 0, 0)},
	{net.IPv4(255, 255, 255, 255), net.IPv4Mask(255, 255, 255, 255)},
}

// SQLite query.
const query = `SELECT
  city_location.country_code,
  country_blocks.country_name,
  city_location.region_code,
  region_names.region_name,
  city_location.city_name,
  city_location.postal_code,
  city_location.latitude,
  city_location.longitude,
  city_location.metro_code,
  city_location.area_code
FROM city_blocks
  NATURAL JOIN city_location
  INNER JOIN country_blocks ON
    city_location.country_code = country_blocks.country_code
  LEFT OUTER JOIN region_names ON
    city_location.country_code = region_names.country_code
    AND
    city_location.region_code = region_names.region_code
WHERE city_blocks.ip_start <= ?
ORDER BY city_blocks.ip_start DESC LIMIT 1`

// Quota interface for limiting access to the API.
type Quota interface {
	Setup(args ...string)          // Initialize quota backend
	Ok(ipkey uint32) (bool, error) // Returns true if under quota
}

// MapQuota implements the Quota interface using a map as the backend.
type MapQuota struct {
	mu sync.Mutex
	m  map[uint32]int
}

func (q *MapQuota) Setup(args ...string) {
	q.m = make(map[uint32]int)
}

func (q *MapQuota) Ok(ipkey uint32) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n, ok := q.m[ipkey]; ok {
		if n < conf.Limit.MaxRequests {
			q.m[ipkey]++
			return true, nil
		}
		return false, nil
	}
	q.m[ipkey] = 1
	go func() {
		time.Sleep(time.Duration(conf.Limit.Expire) * time.Second)
		q.mu.Lock()
		defer q.mu.Unlock()
		delete(q.m, ipkey)
	}()
	return true, nil
}

// RedisQuota implements the Quota interface using Redis as the backend.
type RedisQuota struct {
	c *redis.Client
}

func (q *RedisQuota) Setup(args ...string) {
	q.c = redis.New(args...)
	q.c.Timeout = time.Duration(800) * time.Millisecond
}

func (q *RedisQuota) Ok(ipkey uint32) (bool, error) {
	k := fmt.Sprintf("%d", ipkey) // "numeric" key
	if ns, err := q.c.Get(k); err != nil {
		return false, fmt.Errorf("redis get: %s", err.Error())
	} else if ns == "" {
		if err = q.c.SetEx(k, conf.Limit.Expire, "1"); err != nil {
			return false, fmt.Errorf("redis setex: %s", err.Error())
		}
	} else if n, _ := strconv.Atoi(ns); n < conf.Limit.MaxRequests {
		if n, err = q.c.Incr(k); err != nil {
			return false, fmt.Errorf("redis incr: %s", err.Error())
		} else if n == 1 {
			q.c.Expire(k, conf.Limit.Expire)
		}
	} else {
		return false, nil
	}
	return true, nil
}
