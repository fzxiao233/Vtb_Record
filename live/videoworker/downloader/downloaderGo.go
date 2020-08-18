package downloader

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"github.com/bitly/go-simplejson"
	"github.com/fzxiao233/Vtb_Record/config"
	"github.com/fzxiao233/Vtb_Record/live/interfaces"
	"github.com/fzxiao233/Vtb_Record/utils"
	"github.com/hashicorp/golang-lru"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
	"github.com/valyala/bytebufferpool"
	"go.uber.org/ratelimit"
	"golang.org/x/sync/semaphore"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"

	//"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

type DownloaderGo struct {
	Downloader
	cookie string
	proxy  string
	useAlt bool
}

func doDownloadHttp(entry *log.Entry, output string, url string, headers map[string]string, needMove bool) error {
	// Create the file
	/*out, err := os.Create(output)
	if err != nil {
		return err
	}
	if !needMove {
		defer func () {
			go out.Close()
		}()
	} else {
		defer out.Close()
	}*/
	out := utils.GetWriter(output)
	defer out.Close()

	transport := &http.Transport{}

	client := &http.Client{
		Transport: transport,
	}
	// Get the data
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check server response
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloader got bad status: %s", resp.Status)
	}

	buf := make([]byte, 1024*1024*3) // 1M buffer
	src := resp.Body
	dst := out
	for {
		// Writer the body to file
		written := int64(0)
		for {
			nr, er := src.Read(buf)
			if nr > 0 {
				nw, ew := dst.Write(buf[0:nr])
				if nw > 0 {
					written += int64(nw)
				}
				if ew != nil {
					err = ew
					break
				}
				if nr != nw {
					err = io.ErrShortWrite
					break
				}
			}
			if er != nil {
				err = er
				break
			}
		}

		//written, err := io.CopyBuffer(out, resp.Body, buf)
		entry.Infof("Wrote %s, err: %s", written, err)
		if err == nil {
			return nil
		} else if err == io.EOF {
			entry.Info("Stream ended")
			return nil
		} else {
			return err
		}
	}

	return nil
}

type HLSSegment struct {
	SegNo         int
	SegArriveTime time.Time
	Url           string
	//Data          []byte
	Data *bytes.Buffer
}

type HLSDownloader struct {
	Logger          *log.Entry
	M3U8UrlRewriter URLRewriter
	AltAsMain       bool
	OutPath         string
	Video           *interfaces.VideoInfo
	Cookie          string

	HLSUrl         string
	HLSHeader      map[string]string
	AltHLSUrl      string
	AltHLSHeader   map[string]string
	UrlUpdating    sync.Mutex
	AltUrlUpdating sync.Mutex

	Clients    []*http.Client
	AltClients []*http.Client
	allClients []*http.Client

	SeqMap     sync.Map
	AltSeqMap  *lru.Cache
	FinishSeq  int
	lastSeqNo  int
	Stopped    bool
	AltStopped bool
	output     io.Writer
	segRl      ratelimit.Limiter

	firstSeqChan chan int

	errChan    chan error
	alterrChan chan error

	forceRefreshChan    chan int
	altforceRefreshChan chan int

	downloadErr    *cache.Cache
	altdownloadErr *cache.Cache
}

var bufPool bytebufferpool.Pool

var IsStub = false

func (d *HLSDownloader) handleSegment(segData *HLSSegment) bool {
	return d._handleSegment(segData, false)
}

func (d *HLSDownloader) handleAltSegment(segData *HLSSegment) bool {
	return d._handleSegment(segData, true)
}

// download each segment
func (d *HLSDownloader) _handleSegment(segData *HLSSegment, isAlt bool) bool {
	d.segRl.Take()
	if IsStub {
		return true
	}

	logger := d.Logger.WithField("alt", isAlt)
	//downChan := make(chan []byte)
	downChan := make(chan *bytes.Buffer)
	defer func() {
		defer func() {
			recover()
		}()
		close(downChan)
	}()
	doDownload := func(client *http.Client) {
		//buf := bufPool.Get()
		s := time.Now()
		newbuf, err := utils.HttpGetBuffer(client, segData.Url, d.HLSHeader, nil)
		//buf := make([]byte, 6 * 1024 * 1024)
		//buf := make([]byte, 1)
		//Read(buf)
		//newbuf := bytes.NewBuffer(buf)
		//var err error
		if err != nil {
			logger.WithError(err).Infof("Err when download segment %s", segData.Url)
			if strings.HasSuffix(err.Error(), "404") {
				func() {
					defer func() {
						recover()
					}()
					ch := downChan
					if ch == nil {
						return
					}
					ch <- nil
				}()
			}
			//bufPool.Put(buf)
		} else {
			usedTime := time.Now().Sub(s)
			if usedTime > time.Second*15 {
				logger.Infof("Download %d used %s", segData.SegNo, usedTime)
			}
			func() {
				defer func() {
					recover()
				}()
				ch := downChan
				if ch == nil {
					return
				}
				ch <- newbuf
			}()
		}
	}
	onlyAlt := false
	// gotcha104 is tencent yun, only m3u8 blocked the foreign ip, so after that we simply ignore it
	/*if strings.Contains(segData.Url, "gotcha104") {
		onlyAlt = true
	}*/
	i := 0
	clients := d.allClients
	if onlyAlt {
		clients = d.AltClients
		if len(clients) == 0 {
			clients = d.allClients
		}
	} else {
		// TODO: Refactor this
		if strings.Contains(segData.Url, "gotcha105") {
			clients = make([]*http.Client, 0)
			clients = append(clients, d.Clients...)
			clients = append(clients, d.Clients...) // double same client
		} else if strings.Contains(segData.Url, "gotcha104") {
			clients = []*http.Client{}
			clients = append(clients, d.AltClients...)
			clients = append(clients, d.Clients...)
		} else if strings.Contains(segData.Url, "googlevideo.com") {
			clients = []*http.Client{}
			clients = append(clients, d.Clients...)
		}
	}
	round := 0
breakout:
	for {
		i %= len(clients)
		go doDownload(clients[i])
		//go d.downloadWorker(d.allClients[i], segData.Url, downChan)
		i += 1
		select {
		case ret := <-downChan:
			close(downChan)
			if ret == nil { // unrecoverable error, so reture at once
				return false
			}
			segData.Data = ret
			break breakout
		case <-time.After(15 * time.Second):
			// wait 10 second for each download try
		}
		if i == len(clients) {
			logger.Warnf("Failed all-clients to download segment %d", segData.SegNo)
			round++
		}
		if isAlt {
			if round == 2 {
				logger.Warnf("Failed to download alt segment %d after 2 round, giving up")
				return true // true but not setting segment, so not got removed
			}
		}
		if time.Now().Sub(segData.SegArriveTime) > 300*time.Second {
			logger.Warnf("Failed to download segment %d within timeout...", segData.SegNo)
			return false
		}
	}
	if round > 0 || isAlt {
		logger.Infof("Downloaded segment %d: len %v", segData.SegNo, segData.Data.Len())
	} else {
		logger.Debugf("Downloaded segment %d: len %v", segData.SegNo, segData.Data.Len())
	}
	return true
}

// parse the m3u8 file to get segment number and url
func (d *HLSDownloader) m3u8Parser(logger *log.Entry, parsedurl *url.URL, m3u8 string, isAlt bool) bool {
	relaUrl := "http" + "://" + parsedurl.Host + path.Dir(parsedurl.Path)
	hostUrl := "http" + "://" + parsedurl.Host
	getSegUrl := func(url string) string {
		if strings.HasPrefix(url, "http") {
			return url
		} else if url[0:1] == "/" {
			return hostUrl + url
		} else {
			return relaUrl + "/" + url
		}
	}

	m3u8lines := strings.Split(m3u8, "\n")
	if m3u8lines[0] != "#EXTM3U" {
		logger.Warnf("Failed to parse m3u8, expected %s, got %s", "#EXTM3U", m3u8lines[0])
		return false
	}

	curseq := -1
	segs := make([]string, 0)
	i := 0
	finished := false
	for {
		i += 1
		if i >= len(m3u8lines) {
			break
		}
		line := m3u8lines[i]
		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE") {
			_, _, val := utils.RPartition(line, ":")
			_seq, err := strconv.Atoi(val)
			if err != nil {
				logger.Warnf("EXT-X-MEDIA-SEQUENCE malformed: %s", line)
				continue
			}
			curseq = _seq
		} else if strings.HasPrefix(line, "#EXTINF:") {
			//logger.Debugf("Got seg %d %s", curseq+len(segs), m3u8lines[i+1])
			segs = append(segs, m3u8lines[i+1])
			i += 1
		} else if strings.HasPrefix(line, "#EXT-X-ENDLIST") {
			logger.Debug("Got HLS end mark!")
			finished = true
		} else if line == "" || strings.HasPrefix(line, "#EXT-X-VERSION") ||
			strings.HasPrefix(line, "#EXT-X-ALLOW-CACHE") ||
			strings.HasPrefix(line, "#EXT-X-TARGETDURATION") {
		} else {
			logger.Debugf("Ignored line: %s", line)
		}
	}

	if curseq == -1 {
		// curseq parse failed
		logger.Warnf("curseq parse failed!!!")
		return false
	}
	if !isAlt && d.firstSeqChan != nil {
		d.firstSeqChan <- curseq
		d.firstSeqChan = nil
	}
	if !isAlt {
		d.lastSeqNo = curseq + len(segs)
	}
	for i, seg := range segs {
		seqNo := curseq + i
		if !isAlt {
			_segData, loaded := d.SeqMap.LoadOrStore(curseq+i, &HLSSegment{SegNo: seqNo, SegArriveTime: time.Now(), Url: getSegUrl(seg)})
			if !loaded {
				segData := _segData.(*HLSSegment)
				logger.Debugf("Got new seg %d %s", seqNo, segData.Url)
				go d.handleSegment(segData)
			}
		} else {
			d.AltSeqMap.PeekOrAdd(curseq+i, &HLSSegment{SegNo: seqNo, SegArriveTime: time.Now(), Url: getSegUrl(seg)})
		}
	}
	if finished {
		d.FinishSeq = curseq + len(segs) - 1
	}
	return true
}

func (d *HLSDownloader) forceRefresh(isAlt bool) {
	defer func() {
		recover()
	}()
	ch := d.forceRefreshChan
	if !isAlt {
		ch = d.forceRefreshChan
	} else {
		ch = d.altforceRefreshChan
	}
	if ch == nil {
		return
	}
	ch <- 1
}

func (d *HLSDownloader) sendErr(err error) {
	defer func() {
		recover()
	}()
	ch := d.errChan
	if ch == nil {
		return
	}
	ch <- err
}

// the core worker that download the m3u8 file
func (d *HLSDownloader) m3u8Handler(isAlt bool) error {
	logger := d.Logger.WithField("alt", isAlt)
	//m3u8retry := 0
	errCache := d.downloadErr
	if isAlt {
		errCache = d.altdownloadErr
	}
	errCache.DeleteExpired()
	if errCache.ItemCount() >= 3 {
		errs := make([]interface{}, 0, 10)
		for _, e := range errCache.Items() {
			errs = append(errs, e)
		}
		errCache.Flush()
		url := d.HLSUrl
		if isAlt {
			url = d.AltHLSUrl
		}
		logger.WithField("errors", errs).Warnf("Too many err occured downloading %s, refreshing m3u8url...", url)
		d.forceRefresh(isAlt)
		//time.Sleep(5 * time.Second)
	}

	retchan := make(chan []byte, 1)
	defer func() {
		defer func() {
			recover()
		}()
		close(retchan)
	}()

	if retchan == nil {
		retchan = make(chan []byte, 1)
	}

	var curUrl string
	var curHeader map[string]string
	if !isAlt {
		d.UrlUpdating.Lock()
		curUrl = d.HLSUrl
		curHeader = d.HLSHeader
		d.UrlUpdating.Unlock()
	} else {
		d.AltUrlUpdating.Lock()
		curUrl = d.AltHLSUrl
		curHeader = d.AltHLSHeader
		d.AltUrlUpdating.Unlock()
	}

	if curUrl == "" {
		logger.Infof("got empty m3u8 url", curUrl)
		d.forceRefresh(isAlt)
		time.Sleep(10 * time.Second)
		return nil
	}

	// Get the data
	var err error
	var _m3u8 []byte

	parsedurl, err := url.Parse(curUrl)
	if err != nil {
		logger.WithError(err).Warnf("m3u8 url parse fail")
		d.forceRefresh(isAlt)
		//time.Sleep(10 * time.Second)
		return nil
	}

	curUrl, useMain, useAlt := d.M3U8UrlRewriter.rewrite(curUrl)

	//finished := false

	//var errMu sync.Mutex
	//errList := make([]error, 0, 10)
	doQuery := func(client *http.Client) {
		//start := time.Now()
		if _, ok := curHeader["Accept-Encoding"]; ok { // if there's custom Accept-Encoding, http.Client won't process them for us
			delete(curHeader, "Accept-Encoding")
		}
		_m3u8, err = utils.HttpGet(client, curUrl, curHeader)
		if err != nil {
			d.M3U8UrlRewriter.callback(curUrl, err)
			logger.WithError(err).Debugf("Download m3u8 failed")

			if strings.HasSuffix(err.Error(), "404") {
				func() {
					defer func() {
						recover()
					}()
					ch := retchan
					if ch == nil {
						return
					}
					ch <- nil // abort!
				}()
				return
			}
			/*errMu.Lock()
			errList = append(errList, err)
			errMu.Unlock()*/
			if !isAlt {
				d.downloadErr.SetDefault(strconv.Itoa(int(time.Now().Unix())), err)
			} else {
				d.altdownloadErr.SetDefault(strconv.Itoa(int(time.Now().Unix())), err)
			}
		} else {
			func() {
				defer func() {
					recover()
				}()
				ch := retchan
				if ch == nil {
					return
				}
				ch <- _m3u8 // abort!
			}()
			//logger.Debugf("Downloaded m3u8 in %s", time.Now().Sub(start))
			m3u8 := string(_m3u8)
			ret := d.m3u8Parser(logger, parsedurl, m3u8, isAlt)
			if ret {
				//m3u8retry = 0
			} else {
				logger.Warnf("Failed to parse m3u8: %s", m3u8)
				//continue
			}
		}
	}

	//clients := d.allClients
	clients := []*http.Client{}
	if useMain == 0 {
		clients = append(clients, d.AltClients...)
	} else if useAlt == 0 {
		clients = append(clients, d.Clients...)
	} else {
		if useAlt > useMain {
			clients = append(clients, d.AltClients...)
			clients = append(clients, d.Clients...)
		} else {
			clients = d.allClients
		}
	}
	if len(clients) == 0 {
		clients = d.allClients
	}
	// for gotcha105 & gotcha104, never use altproxy when downloading
	/*if strings.Contains(curUrl, "gotcha105") {
		clients = d.Clients
	} else if strings.Contains(curUrl, "baidubce") {
		clients = d.Clients
	} else if strings.Contains(curUrl, "gotcha104") {
		clients = []*http.Client{}
		clients = append(clients, d.AltClients...)
		clients = append(clients, d.Clients...)
	}*/

breakout:
	for i, client := range clients {
		go doQuery(client)
		select {
		case ret := <-retchan:
			close(retchan)
			retchan = nil
			if ret == nil {
				//logger.Info("Unrecoverable m3u8 download err, aborting")
				return fmt.Errorf("Unrecoverable m3u8 download err, aborting, url: %s", curUrl)
			}
			_m3u8 = ret
			if !isAlt {
				d.downloadErr.Flush()
			} else {
				d.altdownloadErr.Flush()
			}
			break breakout
		case <-time.After(time.Millisecond * 2500): // failed to download within timeout, issue another req
			logger.Debugf("Download m3u8 %s timeout with client %d", curUrl, i)
		}
	}
	return nil
}

// download main m3u8 every 2 seconds
func (d *HLSDownloader) Downloader() {
	ticker := time.NewTicker(time.Second * 2)
	defer ticker.Stop()
	breakflag := false
	for {
		go func() {
			err := d.m3u8Handler(false)
			if err != nil {
				d.sendErr(err)
				breakflag = true
				return
			}
		}()
		if breakflag {
			return
		}
		if d.FinishSeq > 0 {
			d.Stopped = true
		}
		if d.Stopped {
			break
		}
		<-ticker.C
	}
}

// download alt m3u8 every 3 seconds
func (d *HLSDownloader) AltDownloader() {
	ticker := time.NewTicker(time.Second * 3)
	defer ticker.Stop()
	for {
		err := d.m3u8Handler(true)
		if err != nil {
			d.Logger.WithError(err).Infof("Alt m3u8 download failed")
		}
		if d.AltStopped {
			break
		}
		<-ticker.C
	}
}

// update the main hls stream's link
func (d *HLSDownloader) Worker() {
	ticker := time.NewTicker(time.Minute * 40)
	defer ticker.Stop()
	for {
		if d.forceRefreshChan == nil {
			d.forceRefreshChan = make(chan int)
		}
		if d.Stopped {
			<-ticker.C
		} else {
			select {
			case _ = <-ticker.C:

			case _ = <-d.forceRefreshChan:
				d.Logger.Info("Got forceRefresh signal, refresh at once!")
				isClose := false
				func() {
					defer func() {
						panicMsg := recover()
						if panicMsg != nil {
							isClose = true
						}
					}()
					close(d.forceRefreshChan)
					d.forceRefreshChan = nil // avoid multiple refresh
				}()
				if isClose {
					return
				}
			}
		}
		retry := 0
		for {
			retry += 1
			if retry > 1 {
				time.Sleep(30 * time.Second)
				if retry > 20 {
					d.sendErr(fmt.Errorf("failed to update playlist in 20 attempts"))
					return
				}
				if d.Stopped {
					return
				}
			}
			alt := d.AltAsMain
			needAbort, err, infoJson := updateInfo(d.Video, "", d.Cookie, alt)
			if needAbort {
				d.Logger.WithError(err).Warnf("Streamlink requests to abort, worker finishing...")
				return
			}
			if err != nil {
				d.Logger.WithError(err).Warnf("Failed to update playlist")
				continue
			}
			m3u8url, headers, err := parseHttpJson(infoJson)
			if err != nil {
				d.Logger.WithError(err).Warnf("Failed to parse json ret")
				continue
			}

			d.Logger.Infof("Got new m3u8url: %s", m3u8url)
			if m3u8url == "" {
				d.Logger.Warnf("Got empty m3u8 url...: %s", infoJson)
				continue
			}
			d.UrlUpdating.Lock()
			d.HLSUrl = m3u8url
			d.HLSHeader = headers
			d.UrlUpdating.Unlock()
			break
		}
		if d.Stopped {
			return
		}
	}
}

// update the alt hls stream's link
func (d *HLSDownloader) AltWorker() {
	logger := d.Logger.WithField("alt", true)
	ticker := time.NewTicker(time.Minute * 40)
	defer ticker.Stop()

	if d.AltHLSUrl == "" {
		d.AltUrlUpdating.Lock()
		d.AltHLSUrl = d.HLSUrl
		d.AltHLSHeader = d.HLSHeader
		d.AltUrlUpdating.Unlock()
	}

	for {
		if d.altforceRefreshChan == nil {
			time.Sleep(120 * time.Second)
			d.altforceRefreshChan = make(chan int)
		}
		select {
		case _ = <-ticker.C:

		case _ = <-d.altforceRefreshChan:
			logger.Info("Got forceRefresh signal, refresh at once!")
			isClose := false
			func() {
				defer func() {
					panicMsg := recover()
					if panicMsg != nil {
						isClose = true
					}
				}()
				ch := d.altforceRefreshChan
				d.altforceRefreshChan = nil // avoid multiple refresh
				close(ch)
			}()
			if isClose {
				return
			}
		}
		retry := 0
		for {
			retry += 1
			if retry > 1 {
				time.Sleep(30 * time.Second)
				if retry > 5 {
					logger.Warnf("failed to update playlist in 5 attempts, fallback to main hls")
					d.AltUrlUpdating.Lock()
					d.AltHLSUrl = d.HLSUrl
					d.AltHLSHeader = d.HLSHeader
					d.AltUrlUpdating.Unlock()
					return
				}
				if d.AltStopped {
					return
				}
			}
			needAbort, err, infoJson := updateInfo(d.Video, "", d.Cookie, true)
			if needAbort {
				logger.WithError(err).Warnf("Alt streamlink requested to abort")
				for {
					if d.AltStopped {
						return
					}
					time.Sleep(10 * time.Second)
				}
			}
			if err != nil {
				logger.Warnf("Failed to update playlist: %s", err)
				continue
			}
			m3u8url, headers, err := parseHttpJson(infoJson)
			if err != nil {
				logger.WithError(err).Warnf("Failed to parse json, rawData: %s", infoJson)
				continue
			}

			logger.Infof("Got new m3u8url: %s", m3u8url)
			if m3u8url == "" {
				logger.Warnf("Got empty m3u8 url...: %s", infoJson)
				continue
			}
			// if we only have qiniu
			if strings.Contains(m3u8url, "gotcha103") {
				//fuck qiniu
				logger.Infof("We got qiniu cdn... %s", m3u8url)
				/*.time.AfterFunc(300*time.Second, func() {
					d.forceRefresh(true)
				})*/
				// if we have different althlsurl, then we've got other cdn other than qiniu cdn, so we retry!
				url1 := d.HLSUrl[strings.Index(d.HLSUrl, "://")+3:]
				url2 := d.AltHLSUrl[strings.Index(d.AltHLSUrl, "://")+3:]
				urlhost1 := url1[:strings.Index(url1, "/")]
				urlhost2 := url2[:strings.Index(url2, "/")]
				if urlhost1 == urlhost2 {
					m3u8url = d.HLSUrl
					headers = d.HLSHeader
				} else {
					logger.Infof("We got a good alt m3u8 before: %s, not replacing it", d.AltHLSUrl)
					m3u8url = ""
					time.Sleep(270 * time.Second) // additional sleep time for this reason
					continue                      // use the retry logic
				}
			}

			if m3u8url != "" {
				logger.Infof("Updated AltHLSUrl: %s", m3u8url)
				d.AltUrlUpdating.Lock()
				d.AltHLSUrl = m3u8url
				d.AltHLSHeader = headers
				d.AltUrlUpdating.Unlock()
			}
			break
		}
		if d.AltStopped {
			return
		}
	}
}

func (d *HLSDownloader) WriterStub() {
	for {
		timer := time.NewTimer(time.Second * time.Duration((50+rand.Intn(20))/10))
		d.output.Write(randData)
		<-timer.C
	}
}

// Responsible to write out each segments
func (d *HLSDownloader) Writer() {
	curSeq := <-d.firstSeqChan
	//firstSeq := curSeq
	for {
		loadTime := time.Second * 0
		//d.Logger.Debugf("Loading segment %d", curSeq)
		for {
			_val, ok := d.SeqMap.Load(curSeq)
			if ok {
				val := _val.(*HLSSegment)
				if curSeq >= 30 {
					d.SeqMap.Delete(curSeq - 30)
				}

				if val.Data != nil {
					timeoutChan := make(chan int, 1)
					go func(timeoutChan chan int, startTime time.Time, segNo int) {
						timer := time.NewTimer(15 * time.Second)
						select {
						case <-timeoutChan:
							d.Logger.Debugf("Wrote segment %d in %s", segNo, time.Now().Sub(startTime))
						case <-timer.C:
							d.Logger.Warnf("Write segment %d too slow...", curSeq)
							timer2 := time.NewTimer(60 * time.Second)
							select {
							case <-timeoutChan:
								d.Logger.Debugf("Wrote segment %d in %s", segNo, time.Now().Sub(startTime))
							case <-timer2.C:
								d.Logger.Errorf("Write segment %d timeout!!!!!!!", curSeq)
							}
						}
					}(timeoutChan, time.Now(), curSeq)
					_, err := d.output.Write(val.Data.Bytes())
					//_, err := d.output.Write(make([]byte, 6 * 1024 * 1024))
					timeoutChan <- 1
					//bufPool.Put(val.Data)
					val.Data = nil
					if err != nil {
						d.sendErr(err)
						return
					}
					break
				}
			} else {
				isLagged := false
				if d.lastSeqNo > 3 && d.lastSeqNo+2 < curSeq { // seqNo got reset to 0
					// exit ASAP so that alt stream will be preserved
					d.sendErr(fmt.Errorf("Failed to load segment %d due to segNo got reset to %d", curSeq, d.lastSeqNo))
					return
				} else {
					d.SeqMap.Range(func(key, value interface{}) bool {
						if key.(int) > curSeq+3 && value.(*HLSSegment).Data != nil {
							isLagged = true
							return false
						} else {
							return true
						}
					})
					if isLagged && loadTime > 15*time.Second { // exit ASAP so that alt stream will be preserved
						d.sendErr(fmt.Errorf("Failed to load segment %d within m3u8 timeout due to lag...", curSeq))
						return
					}
				}
			}
			time.Sleep(500 * time.Millisecond)
			loadTime += 500 * time.Millisecond
			if loadTime == 1*time.Minute || loadTime == 150*time.Second || loadTime == 240*time.Second {
				go d.AltSegDownloader() // trigger alt download in advance, so we can avoid more loss
			}
			if loadTime > 5*time.Minute { // segNo shouldn't return to 0 within 5 min
				d.sendErr(fmt.Errorf("Failed to load segment %d within timeout...", curSeq))
				return
			}
			if curSeq == d.FinishSeq { // successfully finished
				d.sendErr(nil)
				return
			}
		}
		curSeq += 1
	}
}

// Download the segments located in the alt cache
func (d *HLSDownloader) AltSegDownloader() {
	for _, _segNo := range d.AltSeqMap.Keys() {
		segNo := _segNo.(int)
		_segData, ok := d.AltSeqMap.Peek(segNo)
		if ok {
			segData := _segData.(*HLSSegment)
			if segData.Data == nil {
				go func(segNo int, segData *HLSSegment) {
					if segData.Data == nil {
						ret := d.handleAltSegment(segData)
						if !ret {
							d.AltSeqMap.Remove(segNo)
						}
					}
				}(segNo, segData)
				time.Sleep(1 * time.Second)
			}
		}
	}
}

var AltSemaphore = semaphore.NewWeighted(12)

// AltWriter writes the alt hls stream's segments into _tail.ts files
func (d *HLSDownloader) AltWriter() {
	AltSemaphore.Acquire(context.Background(), 1)
	defer AltSemaphore.Release(1)
	defer d.AltSeqMap.Purge()

	if d.AltSeqMap.Len() == 0 {
		d.AltStopped = true
		return
	}
	writer := utils.GetWriter(utils.AddSuffix(d.OutPath, "tail"))
	defer writer.Close()
	d.Logger.Infof("Started to write tail video!")

	d.AltSegDownloader()
	time.Sleep(15 * time.Second)
	d.AltStopped = true
	func() {
		defer func() {
			recover()
		}()
		close(d.altforceRefreshChan)
	}()
	d.AltSegDownloader()
	time.Sleep(20 * time.Second)
	segs := []int{}
	for _, _segNo := range d.AltSeqMap.Keys() {
		segNo := _segNo.(int)
		_segData, ok := d.AltSeqMap.Peek(segNo)
		if ok {
			if _segData.(*HLSSegment).Data != nil {
				segs = append(segs, segNo)
			}
		}
	}
	d.Logger.Infof("Got tail segs: %s", segs)

	min := 10000000000
	max := -1000
	for _, v := range d.AltSeqMap.Keys() {
		if v.(int) < min {
			min = v.(int)
		}
		if v.(int) > max {
			max = v.(int)
		}
	}

	// sometimes the cdn will reset everything back to 1 and then restart, so after wrote the
	// last segments, we try to write the first parts
	resetNo := 0
	if min < 25 {
		for i := min; i < 25; i++ {
			if seg, ok := d.AltSeqMap.Peek(i); ok {
				if seg.(*HLSSegment).Data != nil {
					resetNo = i + 1
					continue
				}
			}
			break
		}
	}

	startNo := min
	lastGood := min
	for i := startNo; i <= max; i++ {
		if seg, ok := d.AltSeqMap.Peek(i); ok {
			if seg.(*HLSSegment).Data != nil {
				lastGood = startNo
				continue
			}
		}
		startNo = i
	}
	if startNo > max {
		startNo = lastGood
	}
	d.Logger.Infof("Going to write segment %d to %d", startNo, max)
	var i int
	for i = startNo + 1; i <= max; i++ {
		if _seg, ok := d.AltSeqMap.Peek(i); ok {
			seg := _seg.(*HLSSegment)
			if seg.Data != nil {
				_, err := writer.Write(seg.Data.Bytes())
				//bufPool.Put(seg.Data)
				seg.Data = nil
				if err != nil {
					d.Logger.Warnf("Failed to write to tail video, err: %s", err)
					return
				}
				continue
			}
		}
		break
	}

	d.Logger.Infof("Finished writing segment %d to %d", startNo+1, i)
	if resetNo != 0 {
		for i := min; i < resetNo; i++ {
			if _seg, ok := d.AltSeqMap.Peek(i); ok {
				seg := _seg.(*HLSSegment)
				if seg.Data != nil {
					_, err := writer.Write(seg.Data.Bytes())
					//bufPool.Put(seg.Data)
					seg.Data = nil
					if err != nil {
						d.Logger.Warnf("Failed to write to tail video, err: %s", err)
						return
					}
					continue
				}
			}
			break
		}
		d.Logger.Infof("Finished writing reset segment %d to %d", 1, resetNo-1)
	}
}

func (d *HLSDownloader) startDownload() error {
	var err error

	// rate limit, so we won't break up all things
	d.segRl = ratelimit.New(1)

	writer := utils.GetWriter(d.OutPath)
	d.output = writer
	defer writer.Close()

	d.allClients = make([]*http.Client, 0)
	d.allClients = append(d.allClients, d.Clients...)
	d.allClients = append(d.allClients, d.AltClients...)

	d.AltSeqMap, _ = lru.New(16)
	d.errChan = make(chan error)
	d.alterrChan = make(chan error)
	d.firstSeqChan = make(chan int)
	d.forceRefreshChan = make(chan int)
	d.altforceRefreshChan = make(chan int)
	d.downloadErr = cache.New(30*time.Second, 5*time.Minute)
	d.altdownloadErr = cache.New(30*time.Second, 5*time.Minute)

	/*err, altinfoJson := updateInfo(d.Video, "", d.Cookie, true)
	if err == nil {
		alturl, altheaders, err := parseHttpJson(altinfoJson)
		if err == nil {
			d.AltHLSUrl = alturl
			d.AltHLSHeader = altheaders
		}
	}*/

	hasAlt := false
	if _, ok := d.Video.UsersConfig.ExtraConfig["AltStreamLinkArgs"]; ok {
		hasAlt = true
	}

	if IsStub {
		hasAlt = false
		go d.WriterStub()
	} else {
		go d.Writer()
	}

	go d.Downloader()
	go d.Worker()

	if hasAlt {
		d.Logger.Infof("Use alt downloader")
		go func() {
			for {
				d.AltWorker()
				if d.AltStopped {
					break
				}
			}
		}()
		d.altforceRefreshChan <- 1
		time.AfterFunc(30*time.Second, d.AltDownloader)
	} else {
		d.Logger.Infof("Disabled alt downloader")
	}

	startTime := time.Now()
	err = <-d.errChan
	usedTime := time.Now().Sub(startTime)
	if err == nil {
		d.Logger.Infof("HLS Download successfully!")
		d.AltStopped = true
	} else {
		d.Logger.Infof("HLS Download failed: %s", err)
		if hasAlt {
			if usedTime > 1*time.Minute {
				go d.AltWriter()
			} else {
				d.AltStopped = true
			}
		}
	}
	func() {
		defer func() {
			recover()
		}()
		close(d.errChan)
		close(d.forceRefreshChan)
	}()
	d.Stopped = true
	d.SeqMap = sync.Map{}
	defer func() {
		go func() {
			time.Sleep(3 * time.Minute)
			d.AltStopped = true
		}()
	}()
	return err
}

func (dd *DownloaderGo) doDownloadHls(entry *log.Entry, output string, video *interfaces.VideoInfo, m3u8url string, headers map[string]string, needMove bool) error {
	clients := []*http.Client{
		{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 20 * time.Second,
				TLSNextProto:          make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
				//DisableCompression:    true,
				DisableKeepAlives: false,
			},
			Timeout: 60 * time.Second,
		},
	}

	_altproxy, ok := video.UsersConfig.ExtraConfig["AltProxy"]
	var altproxy string
	var altclients []*http.Client
	if ok {
		altproxy = _altproxy.(string)
		proxyUrl, _ := url.Parse("socks5://" + altproxy)
		altclients = []*http.Client{
			{
				Transport: &http.Transport{
					TLSNextProto: make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
					Proxy:        http.ProxyURL(proxyUrl),
					//DisableCompression: true,
					DisableKeepAlives: false,
				},
				Timeout: 100 * time.Second,
			},
		}
	} else {
		altclients = []*http.Client{}
	}

	d := &HLSDownloader{
		Logger:          entry,
		AltAsMain:       dd.useAlt,
		HLSUrl:          m3u8url,
		HLSHeader:       headers,
		AltHLSUrl:       m3u8url,
		AltHLSHeader:    headers,
		Clients:         clients,
		AltClients:      altclients,
		Video:           video,
		OutPath:         output,
		Cookie:          dd.cookie,
		M3U8UrlRewriter: getRewriter(),
		//output:    out,
	}

	err := d.startDownload()
	time.Sleep(1 * time.Second)
	utils.ExecShell("/home/misty/rclone", "rc", "vfs/forget", "dir="+path.Dir(output))
	return err
}

var rl ratelimit.Limiter
var randData []byte

func init() {
	rl = ratelimit.New(1)
	randFile, err := os.Open("randData")
	if err == nil {
		randData = make([]byte, 6*1024*1024)
		io.ReadFull(randFile, randData)
	}
}

var StreamlinkSemaphore = semaphore.NewWeighted(3)

func updateInfo(video *interfaces.VideoInfo, proxy string, cookie string, isAlt bool) (needAbort bool, err error, infoJson *simplejson.Json) {
	needAbort = false
	rl.Take()
	logger := log.WithField("video", video).WithField("alt", isAlt)
	var conf string
	if isAlt {
		conf = "AltStreamLinkArgs"
	} else {
		conf = "StreamLinkArgs"
	}
	_arg, ok := video.UsersConfig.ExtraConfig[conf]
	arg := []string{}
	if ok {
		for _, a := range _arg.([]interface{}) {
			arg = append(arg, a.(string))
		}
	}
	arg = append(arg, []string{"--json"}...)
	if proxy != "" {
		arg = addStreamlinkProxy(arg, proxy)
	}
	if cookie != "" {
		hasCookie := false
		for _, c := range arg {
			if c == "--http-cookies" {
				hasCookie = true
			}
		}
		if !hasCookie {
			arg = append(arg, []string{"--http-cookies", cookie}...)
		}
	}
	arg = append(arg, video.Target, config.Config.DownloadQuality)
	logger.Infof("start to query, command %s", arg)
	StreamlinkSemaphore.Acquire(context.Background(), 1)
	ret, stderr := utils.ExecShellEx(logger, false, "streamlink", arg...)
	StreamlinkSemaphore.Release(1)
	if stderr != "" {
		logger.Infof("Streamlink err output: %s", stderr)
		if strings.Contains(stderr, "(abort)") {
			err = fmt.Errorf("streamlink requested abort")
			needAbort = true
			return
		}
	}
	if ret == "" {
		err = fmt.Errorf("streamlink returned unexpected json")
		return
	}
	_ret := []byte(ret)
	infoJson, _ = simplejson.NewJson(_ret)
	if infoJson == nil {
		err = fmt.Errorf("JSON parsed failed: %s", ret)
		return
	}
	slErr := infoJson.Get("error").MustString()
	if slErr != "" {
		err = fmt.Errorf("Streamlink error: " + slErr)
		if strings.Contains(stderr, "(abort)") {
			log.WithField("video", video).WithError(err).Warnf("streamlink requested abort")
			needAbort = true
		}
		return
	}
	err = nil
	return
}

func parseHttpJson(infoJson *simplejson.Json) (string, map[string]string, error) {
	jret := infoJson.Get("url")
	if jret == nil {
		return "", nil, fmt.Errorf("Not a good json ret: no url")
	}
	url := jret.MustString()
	headers := make(map[string]string)
	jret = infoJson.Get("headers")
	if jret == nil {
		return "", nil, fmt.Errorf("Not a good json ret: no headers")
	}
	for k, v := range jret.MustMap() {
		headers[k] = v.(string)
	}
	return url, headers, nil
}

func (d *DownloaderGo) StartDownload(video *interfaces.VideoInfo, proxy string, cookie string, filepath string) error {
	logger := log.WithField("video", video)
	d.cookie = cookie
	d.proxy = proxy
	d.useAlt = false

	var err error
	var infoJson *simplejson.Json
	var streamtype string
	var needAbort bool
	for i := 0; i < 6; i++ {
		if i < 3 {
			needAbort, err, infoJson = updateInfo(video, proxy, cookie, false)
		} else {
			d.useAlt = true
			needAbort, err, infoJson = updateInfo(video, proxy, cookie, true)
		}
		if needAbort {
			logger.Warnf("Streamlink requested to abort because: %s", err)
			panic("forceabort")
		}
		if err == nil {
			err = func() error {
				jret := infoJson.Get("type")
				if jret == nil {
					return fmt.Errorf("Not a good json ret: no type")
				}
				streamtype = jret.MustString()
				if streamtype == "" {
					return fmt.Errorf("Not a good json ret: %s", infoJson)
				}
				return nil
			}()
			if err != nil {
				continue
			}
			if streamtype == "http" || streamtype == "hls" {
				url, headers, err := parseHttpJson(infoJson)
				if err != nil {
					return err
				}
				//needMove := config.Config.UploadDir == config.Config.DownloadDir
				needMove := false
				if streamtype == "http" {
					logger.Infof("start to download httpstream %s", url)
					return doDownloadHttp(logger, filepath, url, headers, needMove)
				} else {
					if strings.Contains(url, "gotcha103") {
						//fuck qiniu
						//entry.Errorf("Not supporting qiniu cdn... %s", m3u8url)
						logger.Warnf("Not supporting qiniu cdn... %s", url)
						continue
					}
					logger.Infof("start to download hls stream %s", url)
					return d.doDownloadHls(logger, filepath, video, url, headers, needMove)
				}
			} else {
				return fmt.Errorf("Unknown stream type: %s", streamtype)
			}
		} else {
			logger.Infof("Failed to query m3u8 url with isAlt: %s, err: %s", d.useAlt, err)
			if needAbort {
				return fmt.Errorf("abort")
			}
		}
	}
	return err
}
