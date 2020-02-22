package downloader

import (
	"bytes"
	"context"
	"github.com/pkg/errors"
	"io"
	"log"
	"sync"
)

type eagerReader struct {
	task         *Task
	from         int64
	end          int64
	workerNumber int

	ctx           context.Context
	cancel        context.CancelFunc
	taskMap       map[int64]*eagerReaderTask
	taskMapLock   sync.Mutex
	taskMapCond   sync.Cond
	next          int64 // 接下来从哪里读
	workerNext    int64
	closed        bool
	userReadeLock sync.Mutex
	startConsumed bool
}

type eagerReaderTaskStatus uint8

const (
	eagerDoing eagerReaderTaskStatus = iota
	eagerDone
)

type eagerReaderTask struct {
	from   int64
	status eagerReaderTaskStatus
	buf    *bytes.Buffer
}

func (r *eagerReader) init() {
	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.taskMap = make(map[int64]*eagerReaderTask)
	r.taskMapCond.L = &r.taskMapLock
	r.next = r.from
	r.workerNext = r.next - (r.next%r.task.segmentSize)
	for i := 0; i < r.workerNumber; i++ {
		go r.work()
	}
}

func (r *eagerReader) Read(p []byte) (n int, err error) {
	r.userReadeLock.Lock()
	defer r.userReadeLock.Unlock()

	if r.next > r.end {
		r.cancel()
		return n, io.EOF
	}

	off := r.next % r.task.segmentSize
	segStart := r.next - off

	// 等待下载完成
	r.taskMapLock.Lock()
	var task *eagerReaderTask
	for {
		if r.closed {
			err = errors.New("reader already closed")
			return
		}
		var ok bool
		if task, ok = r.taskMap[segStart]; ok {
			if task.status != eagerDone {
				r.taskMapCond.Wait()
			} else {
				break
			}
		} else {
			r.taskMapCond.Wait()
		}
	}
	r.taskMapLock.Unlock()

	// 消耗多余内容
	if !r.startConsumed {
		r.startConsumed = true
		task.buf.Read(make([]byte, off))
	}

	// 读取内容
	n, err = task.buf.Read(p)

	r.next += int64(n)
	// buffer 读取完毕, 释放资源
	if task.buf.Len() == 0 {
		r.taskMapLock.Lock()
		r.task.manager.releaseBuffer(task.buf)
		delete(r.taskMap, task.from)
		r.taskMapCond.Signal()
		r.taskMapLock.Unlock()
	}
	return
}

func (r *eagerReader) Close() error {
	r.cancel()
	r.closed = true
	return nil
}

func (r *eagerReader) work() {
	w := worker{
		id:      0,
		task:    r.task,
		segInfo: &r.task.seg,
		cancel:  nil,
		ctx:     nil,
	}
	for {
		// 获取任务
		task, err := r.getTask()
		if err != nil {
			log.Println("get task error", task)
			break
		}

		if task.from > r.end {
			break
		}

		w.ctx, w.cancel = context.WithCancel(r.ctx)
		// 如果需要下载则下载
		for r.task.seg.doneOrDl(task.from) {
			err = w.downloadSegs(task.from, r.task.segmentSize, 1)
			if err != nil {
				log.Println("download seg error", err)
				return
			}
		}
		// 从磁盘读取
		err = r.task.readFromDisk(task.from, task.buf)
		if err != nil {
			log.Println("文件读取错误", err)
			r.Close()
			return
		}
		r.taskMapLock.Lock()
		task.status = eagerDone
		r.taskMapCond.Broadcast()
		r.taskMapLock.Unlock()
		if r.ctx.Err() != nil {
			break
		}
	}
}

func (r *eagerReader) getTask() (task *eagerReaderTask, err error) {
	r.taskMapLock.Lock()
	defer r.taskMapLock.Unlock()
	if r.workerNext >= r.task.fileLength {
		err = errors.New("no more task")
		return
	}
	// 等待内容被消费
	for len(r.taskMap) >= r.workerNumber {
		r.taskMapCond.Wait()
		if r.ctx.Err() != nil {
			err = r.ctx.Err()
			return
		}
	}
	// 创建任务
	task = &eagerReaderTask{
		from:   r.workerNext,
		buf:    r.task.manager.getBuffer(),
		status: eagerDoing,
	}
	r.workerNext += r.task.segmentSize
	r.taskMap[task.from] = task
	return
}
