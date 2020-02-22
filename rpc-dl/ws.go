package rpc_dl

import (
	"encoding/json"
	"golang.org/x/net/websocket"
	"io/ioutil"
	"log"
	"runtime/debug"
)

func (r *rpcRouter) wsConnHandler(conn *websocket.Conn) {
	defer conn.Close()
	if r.client != nil {
		r.client.Close()
	}
	r.client = conn
	for {
		bin, err := receiveFullFrame(conn)
		if err != nil {
			log.Println("receive failed:", err)
			break
		}
		go r.handleMessage(bin)
	}
	r.client = nil
}

func (r *rpcRouter) handleMessage(s []byte) {
	defer func() {
		err := recover()
		if err != nil {
			log.Println("处理消息出错", err)
			r.sendToClient(gson{
				"type":    "error",
				"message": string(s),
			})
		}
	}()

	var p gson
	err := json.Unmarshal(s, &p)
	if err != nil {
		log.Println(len(s), err, string(s))
		return
	}
	if p["type"].(string) == "call" {
		action := p["action"].(string)
		if fn, ok := r.actionMap[action]; ok {
			result, err := func() (result interface{}, err error) {
				defer func() {
					if e := recover(); e != nil {
						err = e.(error)
						log.Println("调用路由断点函数出错"+action, e)
						log.Printf("stack %s", debug.Stack())
					}
				}()
				return fn(p["param"].(map[string]interface{}))
			}()
			if err != nil {
				r.sendToClient(gson{
					"type":    "call.result",
					"callId":  p["callId"],
					"success": false,
					"result":  err.Error(),
				})
			} else {
				r.sendToClient(gson{
					"type":    "call.result",
					"callId":  p["callId"],
					"success": true,
					"result":  result,
				})
			}
		} else {
			r.sendToClient(gson{
				"type":    "call.result",
				"callId":  p["callId"],
				"success": false,
				"result":  action + "不存在",
			})
		}
	}

}

func (r *rpcRouter) sendToClient(data interface{}) {
	if data == nil || r.client == nil {
		return
	}
	bin, err := json.Marshal(data)
	if err != nil {
		log.Println(err)
		return
	}
	r.clientWriteLock.Lock()
	defer r.clientWriteLock.Unlock()
	_, err = r.client.Write(bin)
	if err != nil {
		log.Println(err)
	}
}

func receiveFullFrame(ws *websocket.Conn) ([]byte, error) {
	var data []byte
	for {
		var seg []byte
		fin, err := receiveFrame(websocket.Message, ws, &seg)
		if err != nil {
			return nil, err
		}
		data = append(data, seg...)
		if fin {
			break
		}
	}
	return data, nil
}

func receiveFrame(cd websocket.Codec, ws *websocket.Conn, v interface{}) (fin bool, err error) {
again:
	frame, err := ws.NewFrameReader()
	if frame.HeaderReader() != nil {
		bin := make([]byte, 1)
		frame.HeaderReader().Read(bin)
		fin = ((bin[0] >> 7) & 1) != 0
	}
	if err != nil {
		return
	}
	frame, err = ws.HandleFrame(frame)
	if err != nil {
		return
	}
	if frame == nil {
		goto again
	}

	payloadType := frame.PayloadType()

	data, err := ioutil.ReadAll(frame)
	if err != nil {
		return
	}
	return fin, cd.Unmarshal(data, payloadType, v)
}
