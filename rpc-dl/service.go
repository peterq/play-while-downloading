package rpc_dl

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/peterq/play-while-downloading/downloader"
	"github.com/pkg/errors"
	"golang.org/x/net/websocket"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

type service struct {
	dl *downloader.Manager
	r  rpcRouter
}

const segSize = 64 * 1024

func (s *service) serve(l net.Listener) {
	parallel := 64
	s.dl = &downloader.Manager{
		CoroutineNumber:       2,
		SegmentSize:           segSize,
		WroteToDiskBufferSize: segSize,
		LinkResolver:          s.resolveLink,
		MaxConsecutiveSeg:     32,
		EventChan:             nil,
		HttpClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:          parallel,
				MaxConnsPerHost:       parallel,
				ResponseHeaderTimeout: 5 * time.Second,
			},
		},
	}
	s.dl.Init()
	go func() {
		for evt := range s.dl.EventChan {
			go s.handleDownloadEvent(evt)
		}
	}()

	s.r.registerRoute(s)
	mux := http.NewServeMux()
	mux.Handle("/jsonrpc", websocket.Handler(s.r.wsConnHandler))
	mux.HandleFunc("/playVideo", s.playVideo)
	err := http.Serve(l, mux)
	if err != nil {
		panic(err)
	}
}

func (s *service) handleDownloadEvent(event *downloader.DownloadEvent) {
	s.r.sendToClient(gson{
		"type":  "event",
		"event": "task-event",
		"payload": gson{
			"event": event.Event,
			"id":    event.TaskId,
			"data":  event.Data,
		},
	})
	if event.Event == "task.speed" {
		d := event.Data.(gson)
		log.Println(float64(d["progress"].(int64))/1024/1024, "M", float64(d["speed"].(int64))/1024, "k")
		return
	}
	log.Println(fmt.Sprintf("%#v", *event))
}

func (s *service) resolveLink(fileId string) (link string, err error) {
	args := strings.Split(fileId, ".")
	switch args[0] {
	case "link":
		return base64Decode(args[1])
	default:
		err = errors.New("unknown download method: " + args[0])
	}
	return
}

func (s *service) playVideo(writer http.ResponseWriter, request *http.Request) {
	s.dl.HandleHttp(writer, request)
}

func (s *service) addTask(id, url string, headers []interface{}, path string) (string, error) {
	headerMap := map[string]string{}
	for _, str := range headers {
		sp := strings.Split(str.(string), ":")
		headerMap[strings.Trim(sp[0], " ")] = strings.Trim(strings.Join(sp[1:], ":"), " ")
	}
	taskId, err := s.dl.NewTask(id, "link."+base64Encode(url), path, func(request *http.Request) *http.Request {
		for key, value := range headerMap {
			request.Header.Set(key, value)
		}
		return request
	})
	return string(taskId), err
}

func (s *service) startTask(id string) error {
	return s.dl.StartTask(downloader.TaskId(id))
}

func (s *service) pauseTask(id string) error {
	return s.dl.PauseTask(downloader.TaskId(id))
}

func base64Decode(s string) (string, error) {
	bin, err := base64.StdEncoding.DecodeString(s)
	return string(bin), err
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func Start() {
	s := service{}
	go func() {
		time.Sleep(time.Second)
		//task, e := s.addTask("test",
		//	"https://qdcache02.baidupcs.com/file/e36eab85ffacecd6c665097a715209f1?bkt=en-cf7b18a7c51d9078bbd16a36d354fc644d042e25ed15c955af705dc97992fe5cee06dad41a1d4b4e&xcode=6cbb98e413dab8f9d9d03aa70ba11523c7dab765320c2532b14da9ac39245b64205f3a88c48bbc0e3bb87f4bbbfeb249b32890021eb52579&fid=2039951508-250528-231663902578415&time=1566791163&sign=FDTAXGERLQBHSKf-DCb740ccc5511e5e8fedcff06b081203-bqAq09J2%2FG%2Bd%2FuUuFtzaI1O8SNA%3D&to=p4&size=44149322&sta_dx=44149322&sta_cs=3&sta_ft=zip&sta_ct=7&sta_mt=0&fm2=MH%2CYangquan%2CAnywhere%2C%2Czhejiang%2Cany&ctime=1465614837&mtime=1566791128&resv0=cdnback&resv1=0&resv2=&resv3=&vuk=2039951508&iv=2&htype=&randtype=&esl=1&newver=1&newfm=1&secfm=1&flow_ver=3&pkey=en-bef78b517c8d2ce4b41793c8ae12c97422d64368f98efd4d71eaac71354d80adfbf7ce9c9ef9a767&sl=74383434&expires=8h&rt=pr&r=308440550&mlogid=MjAxOTA4MjYwMzQ2MDM1NjksNTY5ZjQxZjMyODg0OGNjM2YzNGE1YzY1MDljNTYzZTcsMTg5MA%3D%3D&vbdid=4260107570&fin=Avada+v4.0.3%E5%AE%98%E6%96%B9%E5%8E%9F%E7%89%88%E5%8E%9F%E5%8C%85.zip&bflag=p4,68,70,66,75,80,p0,7,9,5,14,19-p4&err_ver=1.0&check_blue=1&rtype=1&devuid=569f41f328848cc3f34a5c6509c563e7&dp-logid=5508070876714462187&dp-callid=0.1&hps=1&tsl=1200&csl=1200&csign=eYd%2BvquXPQXd0dSmy0AF2E7fXf4%3D&so=0&ut=1&uter=-1&serv=0&uc=1027691377&ti=51702cd94a4865eee1931f420da3811b0152cdfae8f9fac5&by=themis",
		//	[]interface{}{
		//		"User-Agent: netdisk;2.2.3;pc;pc-mac;10.14.5;macbaiduyunguanjia",
		//		"X-Download-From: baiduyun",
		//	}, "/home/peterq/dev/projects/go/github.com/peterq/pan-light/pan-light/play-while-downloading/1.zip")
		//task, e := s.addTask("test_mp4",
		//	"https://qdcu02.baidupcs.com/file/227396c4ed8c09b56ad235d79c265de4?bkt=en-06f5c65000af0ed653e18cecda97b703131fbfdac05be608e548a6a562a249f9baaa8da57730127b&fid=1456427612-250528-726320185733418&time=1566962423&sign=FDTAXGERQBHSKfWa-DCb740ccc5511e5e8fedcff06b081203-IwJlA%2BrODFO%2FWKc3Ccf2JKIcRYg%3D&to=66&size=1426346251&sta_dx=1426346251&sta_cs=0&sta_ft=mp4&sta_ct=2&sta_mt=0&fm2=MH%2CYangquan%2CAnywhere%2C%2Czhejiang%2Cany&ctime=1566665323&mtime=1566962420&resv0=cdnback&resv1=0&resv2=&resv3=&vuk=1456427612&iv=2&htype=&randtype=&esl=1&newver=1&newfm=1&secfm=1&flow_ver=3&pkey=en-e97cd7974ec737a48f21304eccbd24675772645cc4766efe9f251ca0ee947f2d46d34053c63273da&expires=8h&rt=pr&r=470455109&mlogid=MjAxOTA4MjgwMzIwMjI2ODYsNTY5ZjQxZjMyODg0OGNjM2YzNGE1YzY1MDljNTYzZTcsNDUzMg%3D%3D&vbdid=2530241898&fin=%E6%AD%BBshi2.mp4&bflag=66,70,75,80,5,9,14,19-66&err_ver=1.0&check_blue=1&rtype=1&devuid=569f41f328848cc3f34a5c6509c563e7&dp-logid=5554042864503916254&dp-callid=0.1&hps=1&tsl=0&csl=0&csign=tbQ5Qnngjb5fUtoHEuXlii8CF4c%3D&so=0&ut=1&uter=-1&serv=1&uc=1027691377&ti=26fa64dbec28822431d47a8e73abf09f3006296aa5f66c66305a5e1275657320&by=themis",
		//	[]interface{}{
		//		"User-Agent: netdisk;2.2.3;pc;pc-mac;10.14.5;macbaiduyunguanjia",
		//		"X-Download-From: baiduyun",
		//	}, "/home/peterq/桌面/1/dl.mp4")
		//log.Println(task, e)
		//s.dl.StartTask(downloader.TaskId(task))
	}()

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}
	d := gson{
		"msg":  "play-while-downloading init ok",
		"port": listener.Addr().(*net.TCPAddr).Port,
	}
	bytes, _ := json.Marshal(d)
	fmt.Println(string(bytes))
	s.serve(listener)
}
