package downloader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
)

// 实际下载协程
type worker struct {
	id      int
	task    *Task
	segInfo *segmentsInfo
	cancel  func()
	ctx     context.Context
}

func (w *worker) work() {
	w.ctx, w.cancel = context.WithCancel(context.Background())
	errorNumber := 0
	maxErrorNumber := 2
	// 循环下载片段
WorkLoop:
	for {

		// 检查是否有连续错误
		if errorNumber > maxErrorNumber {
			log.Println("too many errors occurred")
			break
		}

		// 判断是否被取消
		select {
		case <-w.ctx.Done():
			break WorkLoop
		default:
		}

		// 获取新的下载片段
		start, size, num := w.segInfo.dl()
		// 没有新的下载片段, 退出下载循环
		if num == 0 {
			break
		}
		err := w.downloadSegs(start, size, num)

		if err != nil {
			log.Println(err)
			errorNumber++
			continue
		}
		// 本次下载没出错, 错误计数置零
		errorNumber = 0
	}
}

func (w *worker) downloadSegs(start, size int64, num int) (err error) {
	end := start - 1 + size*int64(num)
	if end > w.task.fileLength-1 {
		end = w.task.fileLength - 1
	}
	// 构造请求
	req, _ := http.NewRequest("GET", w.task.finalLink, nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	// 用户回调, 可以修改请求
	req = w.task.requestDecorator(req)
	// 发送请求
	var resp *http.Response
	resp, err = w.task.httpClient.Do(req)
	if err != nil {
		w.segInfo.dlFail(start, num)
		return
	}
	// 读取数据流
	buf := w.task.manager.getBuffer()
	defer w.task.manager.releaseBuffer(buf)
	reader := resp.Body
	defer reader.Close()
	for num > 0 {
		err = w.readSegFromBody(resp.Body, buf)
		if err != nil {
			w.segInfo.dlFail(start, num)
			return
		}
		err = w.task.writeToDisk(start, buf)
		if err != nil {
			w.segInfo.dlFail(start, num)
			return
		}
		w.segInfo.dlSuccess(start)
		num--
		start += size
		if num > 0 {
			canContinue := w.segInfo.canContinue(start, num)
			if !canContinue {
				break
			}
		}
	}
	return
}

func (w *worker) readSegFromBody(reader io.Reader, buf *bytes.Buffer) (err error) {
	s := make([]byte, 1024)
	buffLeft := 0
	// 循环读取流
ReadStream:
	for {
		bin := s[:]
		buffLeft = buf.Cap() - buf.Len()
		if buffLeft < len(s) {
			bin = s[:buffLeft]
		}
		var l int
		// 读取流,with context
		select {
		case <-func() chan bool {
			ch := make(chan bool)
			go func() {
				l, err = reader.Read(bin)
				close(ch)
			}()
			return ch
		}():
		case <-w.ctx.Done():
			err = w.ctx.Err()
			break ReadStream
		}
		if l > 0 { // 有数据, 写入缓存
			buf.Write(bin[:l])
			w.task.addSpeedCount(int64(l))
			if buf.Len() == buf.Cap() || err == io.EOF { // 缓存满了, 或者流尾, 写入磁盘
			    err = nil
				return
			}
		}

		if err == io.EOF {
			err = nil
			break
		}

		if err != nil {
			break
		}
	}
	return
}
