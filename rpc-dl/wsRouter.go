package rpc_dl

import (
	"golang.org/x/net/websocket"
	"sync"
)

type gson = map[string]interface{}

type rpcRouter struct {
	actionMap map[string]func(json gson) (interface{}, error)
	client          *websocket.Conn
	clientWriteLock sync.Mutex
}

func (r *rpcRouter) registerRoute(s *service) {
	r.actionMap = make(map[string]func(json gson) (interface{}, error))

	r.actionMap["add"] = func(p gson) (ret interface{}, err error) {
		ret = int(p["a"].(float64)) + int(p["b"].(float64))
		return
	}

	r.actionMap["task.new"] = func(p gson) (ret interface{}, err error) {
		return s.addTask(
			p["id"].(string),
			p["url"].(string),
			p["headers"].([]interface{}),
			p["path"].(string),
		)
	}

	r.actionMap["task.start"] = func(p gson) (ret interface{}, err error) {
		return nil, s.startTask(
			p["id"].(string),
		)
	}

	r.actionMap["task.pause"] = func(p gson) (ret interface{}, err error) {
		return nil, s.pauseTask(
			p["id"].(string),
		)
	}
}
