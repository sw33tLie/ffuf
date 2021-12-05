package runner

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ffuf/ffuf/pkg/ffuf"
	"github.com/icza/gox/stringsx"
)

//Download results < 5MB
const MAX_DOWNLOAD_SIZE = 5242880

const (
	HOST_KEYWORD     = "{HOST}"
	HOSTPORT_KEYWORD = "{HOSTPORT}" // something.com:port
	PORT_KEYWORD     = "{PORT}"
)

type SimpleRunner struct {
	config *ffuf.Config
	client *http.Client
}

func NewSimpleRunner(conf *ffuf.Config, replay bool) ffuf.RunnerProvider {
	var simplerunner SimpleRunner
	proxyURL := http.ProxyFromEnvironment
	customProxy := ""

	if replay {
		customProxy = conf.ReplayProxyURL
	} else {
		customProxy = conf.ProxyURL
	}
	if len(customProxy) > 0 {
		pu, err := url.Parse(customProxy)
		if err == nil {
			proxyURL = http.ProxyURL(pu)
		}
	}

	simplerunner.config = conf
	simplerunner.client = &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       time.Duration(time.Duration(conf.Timeout) * time.Second),
		Transport: &http.Transport{
			Proxy:               proxyURL,
			MaxIdleConns:        1000,
			MaxIdleConnsPerHost: 500,
			MaxConnsPerHost:     500,
			DialContext: (&net.Dialer{
				Timeout: time.Duration(time.Duration(conf.Timeout) * time.Second),
			}).DialContext,
			TLSHandshakeTimeout: time.Duration(time.Duration(conf.Timeout) * time.Second),
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				Renegotiation:      tls.RenegotiateOnceAsClient,
				ServerName:         conf.SNI,
			},
		}}

	if conf.FollowRedirects {
		simplerunner.client.CheckRedirect = nil
	}
	return &simplerunner
}

func (r *SimpleRunner) Prepare(input map[string][]byte) (ffuf.Request, error) {
	req := ffuf.NewRequest(r.config)

	req.Headers = r.config.Headers
	req.Url = r.config.Url
	req.Opaque = r.config.Opaque
	req.Method = r.config.Method
	req.Data = []byte(r.config.Data)

	for keyword, inputitem := range input {
		req.Method = strings.ReplaceAll(req.Method, keyword, string(inputitem))
		headers := make(map[string]string, len(req.Headers))
		for h, v := range req.Headers {
			var CanonicalHeader string = textproto.CanonicalMIMEHeaderKey(strings.ReplaceAll(h, keyword, string(inputitem)))
			headers[CanonicalHeader] = strings.ReplaceAll(v, keyword, string(inputitem))
		}
		req.Headers = headers
		req.Url = strings.ReplaceAll(req.Url, keyword, string(inputitem))
		req.Opaque = strings.ReplaceAll(req.Opaque, keyword, string(inputitem))
		req.Data = []byte(strings.ReplaceAll(string(req.Data), keyword, string(inputitem)))
	}

	// Needed to extract Host
	tempURL := strings.ReplaceAll(req.Url, HOST_KEYWORD, "")
	tempURL = strings.ReplaceAll(tempURL, PORT_KEYWORD, "")
	tempURL = strings.ReplaceAll(tempURL, HOSTPORT_KEYWORD, "")

	u, err := url.Parse(stringsx.Clean(tempURL))
	if err != nil {
		// Todo: improve
		return req, nil
	}

	var port, host string
	if strings.Contains(u.Host, ":") {
		// Port is not implicit aak 80 or 443
		split := strings.Split(u.Host, ":")
		host = split[0]
		port = split[1]
	} else {
		host = u.Host
		if strings.HasPrefix(req.Url, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}

	// Custom templates (TODO: add for headers too)
	req.Url = strings.ReplaceAll(req.Url, HOST_KEYWORD, host)
	req.Opaque = strings.ReplaceAll(req.Opaque, HOST_KEYWORD, host)
	req.Data = []byte(strings.ReplaceAll(string(req.Data), HOST_KEYWORD, host))

	req.Url = strings.ReplaceAll(req.Url, HOSTPORT_KEYWORD, u.Host)
	req.Opaque = strings.ReplaceAll(req.Opaque, HOSTPORT_KEYWORD, u.Host)
	req.Data = []byte(strings.ReplaceAll(string(req.Data), HOSTPORT_KEYWORD, u.Host))

	req.Url = strings.ReplaceAll(req.Url, PORT_KEYWORD, port)
	req.Opaque = strings.ReplaceAll(req.Opaque, PORT_KEYWORD, port)
	req.Data = []byte(strings.ReplaceAll(string(req.Data), PORT_KEYWORD, port))

	req.Input = input
	return req, nil
}

func (r *SimpleRunner) Execute(req *ffuf.Request) (ffuf.Response, error) {
	var httpreq *http.Request
	var err error
	var rawreq []byte
	data := bytes.NewReader(req.Data)

	var start time.Time
	var firstByteTime time.Duration

	trace := &httptrace.ClientTrace{
		WroteRequest: func(wri httptrace.WroteRequestInfo) {
			start = time.Now() // begin the timer after the request is fully written
		},
		GotFirstResponseByte: func() {
			firstByteTime = time.Since(start) // record when the first byte of the response was received
		},
	}

	httpreq, err = http.NewRequestWithContext(r.config.Context, req.Method, req.Url, data)

	if err != nil {
		return ffuf.Response{}, err
	}

	// set default User-Agent header if not present
	if _, ok := req.Headers["User-Agent"]; !ok {
		req.Headers["User-Agent"] = fmt.Sprintf("%s v%s", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/94.0.4606.61 Safari/537.36", ffuf.Version())
	}

	// Handle Go http.Request special cases
	if _, ok := req.Headers["Host"]; ok {
		httpreq.Host = req.Headers["Host"]
	}

	req.Host = httpreq.Host
	httpreq = httpreq.WithContext(httptrace.WithClientTrace(r.config.Context, trace))
	for k, v := range req.Headers {
		httpreq.Header.Set(k, v)
	}

	if len(r.config.OutputDirectory) > 0 {
		rawreq, _ = httputil.DumpRequestOut(httpreq, true)
	}

	if req.Opaque != "" {
		httpreq.URL.Opaque = req.Opaque
	}

	httpresp, err := r.client.Do(httpreq)
	if err != nil {
		return ffuf.Response{}, err
	}

	resp := ffuf.NewResponse(httpresp, req)
	defer httpresp.Body.Close()

	// Check if we should download the resource or not
	size, err := strconv.Atoi(httpresp.Header.Get("Content-Length"))
	if err == nil {
		resp.ContentLength = int64(size)
		if (r.config.IgnoreBody) || (size > MAX_DOWNLOAD_SIZE) {
			resp.Cancelled = true
			return resp, nil
		}
	}

	if len(r.config.OutputDirectory) > 0 {
		rawresp, _ := httputil.DumpResponse(httpresp, true)
		resp.Request.Raw = string(rawreq)
		resp.Raw = string(rawresp)
	}

	if respbody, err := ioutil.ReadAll(httpresp.Body); err == nil {
		resp.ContentLength = int64(len(string(respbody)))
		resp.Data = respbody
	}

	wordsSize := len(strings.Split(string(resp.Data), " "))
	linesSize := len(strings.Split(string(resp.Data), "\n"))
	resp.ContentWords = int64(wordsSize)
	resp.ContentLines = int64(linesSize)
	resp.Time = firstByteTime

	return resp, nil
}
