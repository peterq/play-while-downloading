package downloader

import (
	"bytes"
	"context"
	"fmt"
	"github.com/pkg/errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type TaskState string

const (
	WaitStart   TaskState = "wait.start"
	WaitResume            = "wait.resume"
	COMPLETED             = "completed"
	STARTING              = "starting"
	DOWNLOADING           = "downloading"
	PAUSING               = "pausing"
	ERRORED               = "errored"
)

type Task struct {
	id               TaskId
	fileId           string // 文件标识
	manager          *Manager
	linkResolver     LinkResolver
	requestDecorator func(*http.Request) *http.Request
	coroutineNumber  int
	segmentSize      int64
	savePath         string // 保存地址
	httpClient       *http.Client

	state      TaskState // 任务当前状态
	lastErr    error     // 保存上次错误
	link       string    // 链接地址
	finalLink  string    // redirect 之后的地址
	speedCount int64     // 用来计算下载速度的计数器, 需要原子操作
	speed      int64     // 上一秒下载平均速度

	fileLength      int64 // 文件总大小
	seg             segmentsInfo
	wroteToDiskLock sync.Mutex
	lastCaptureTime time.Time // 上次快照时间

	workers               map[int]*worker // 工作协程map
	workersLock           sync.Mutex
	fileHandle            *os.File
	cancelSpeedCoroutine  context.CancelFunc
	speedCoroutineContext context.Context
	deleteFileWhenStop    bool // 删除文件标识
}

func (task *Task) Id() TaskId {
	return task.id
}

func (task *Task) pause() error {
	if task.state != DOWNLOADING {
		return errors.New("当前状态不能暂停任务")
	}
	task.updateState(PAUSING)
	for _, w := range task.workers {
		w.cancel()
	}
	return nil
}

// 初始化生成下载状态
func (task *Task) init(isResume bool) (err error) {
	// 文件id -> 下载链接
	task.link, err = task.linkResolver(task.fileId)
	if err != nil {
		return errors.Wrap(err, "获取下载链接错误")
	}
	// 获取redirect之后的链接
	req, err := http.NewRequest("GET", task.link, nil)
	if err != nil {
		return errors.Wrap(err, "无法创建request")
	}
	req = task.requestDecorator(req)
	task.finalLink, err = redirectedLink(req)
	if err != nil {
		return errors.Wrap(err, "获取最终链接错误")
	}
	req.URL, _ = url.Parse(task.finalLink)
	// 判断是否支持断点续传
	var supportRange bool
	task.fileLength, _, supportRange, err = downloadFileInfo(req)
	if err != nil {
		return errors.Wrap(err, "获取文件信息错误")
	}
	if !supportRange {
		return errors.New("该文件不支持并行下载")
	}
	// 打开本地文件
	task.fileHandle, err = os.OpenFile(task.savePath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return errors.Wrap(err, "打开本地文件错误")
	}
	// 初始化worker map
	task.workers = map[int]*worker{}
	if isResume {
		panic("implement me")
	} else {
		// 新添加的任务
		task.seg.init(task.segmentSize, task.fileLength, task.manager.MaxConsecutiveSeg)
	}
	return nil
}

func (task *Task) addSpeedCount(cnt int64) {
	atomic.AddInt64(&task.speedCount, cnt)
}

func (task *Task) speedCalculateCoroutine() {
	t := time.Tick(time.Second)
Loop:
	for {
		select {
		case <-task.speedCoroutineContext.Done():
			atomic.SwapInt64(&task.speedCount, 0)
			break Loop
		case <-t:
			cnt := atomic.SwapInt64(&task.speedCount, 0)
			p := atomic.LoadInt64(&task.seg.downloadCount)
			task.notifyEvent("task.speed", map[string]interface{}{
				"speed":    cnt,
				"progress": p,
			})
		}
	}
}

// 开始一个任务
func (task *Task) start() (err error) {
	if task.state != WaitStart {
		return errors.New("当前状态不能开始任务")
	}
	task.updateState(STARTING)
	go func() error {
		err = task.init(false)
		if err != nil {
			task.lastErr = err
			task.updateState(ERRORED)
			return errors.Wrap(err, "任务初始化出错")
		}
		task.updateState(DOWNLOADING)
		task.speedCoroutineContext, task.cancelSpeedCoroutine = context.WithCancel(context.Background())
		go task.speedCalculateCoroutine()
		for i := 0; i < task.coroutineNumber; i++ {
			task.workers[i] = &worker{
				id:      i,
				task:    task,
				segInfo: &task.seg,
			}
			go func(w *worker) {
				w.work()
				task.onWorkerExit(w)
			}(task.workers[i])
		}
		return err
	}()
	return nil
}

// 写入数据到磁盘
func (task *Task) writeToDisk(from int64, buffer *bytes.Buffer) (err error) {
	task.wroteToDiskLock.Lock()
	defer task.wroteToDiskLock.Unlock()
	_, err = task.fileHandle.Seek(from, io.SeekStart)
	if err != nil {
		return errors.Wrap(err, "文件seek错误")
	}
	//check(from, buffer.Bytes())
	ln := buffer.Len()
	l, err := buffer.WriteTo(task.fileHandle)
	if ln != int(l) {
		log.Println("================", ln, l)
	}
	//log.Println("写入片段", from, l)
	if err != nil {
		return errors.Wrap(err, "文件写入错误")
	}
	task.capture(false)
	return
}

func (task *Task) readFromDisk(from int64, buffer *bytes.Buffer) (err error) {
	task.wroteToDiskLock.Lock()
	defer task.wroteToDiskLock.Unlock()
	_, err = task.fileHandle.Seek(from, io.SeekStart)
	if err != nil {
		return errors.Wrap(err, "文件seek错误")
	}
	ln := task.segmentSize
	if task.seg.lastSeg == from {
		ln = task.seg.lastSegSize
	}
	bin := make([]byte, ln)
	l, err := task.fileHandle.Read(bin)
	log.Println(l)
	if err != nil {
		return errors.Wrap(err, "read error")
	}
	buffer.Write(bin[:l])
	return
}

//var ok *os.File
//func check(from int64, bin []byte)  {
//	var err error
//	if ok == nil {
//		ok, err = os.OpenFile("/home/peterq/桌面/1/ok.mp4", os.O_RDONLY, os.ModePerm)
//		if err != nil {
//			panic(err)
//		}
//	}
//	_, err = ok.Seek(from, io.SeekStart)
//	if err != nil {
//		panic(err)
//	}
//	okBin := make([]byte, len(bin))
//	ok.Read(okBin)
//	if !bytes.Equal(bin, okBin) {
//		log.Println(bin)
//		log.Println(okBin)
//		return
//	}
//	log.Println("same")
//}

// 调用此函数请先锁住task.wroteToDisk
func (task *Task) capture(force bool) {
	if time.Now().Sub(task.lastCaptureTime) < time.Second && !force {
		return
	}
	return
	task.lastCaptureTime = time.Now()

	//task.notifyEvent("task.capture", base64.StdEncoding.EncodeToString(bin))
}

// 当有工作线程退出时的回调
func (task *Task) onWorkerExit(w *worker) {
	task.workersLock.Lock()
	defer task.workersLock.Unlock()
	delete(task.workers, w.id)
	log.Println(fmt.Sprintf("task %s, worker %d exit", task.id, w.id))
	if len(task.workers) == 0 {
		go task.onAllWorkerExit()
	}
}

// 当所有线程退出时的回调
func (task *Task) onAllWorkerExit() {
	log.Println("所有worker结束")
	task.cancelSpeedCoroutine()
	//task.fileHandle.Close()
	st := WaitStart
	if task.seg.downloadCount == task.fileLength {
		st = COMPLETED
	} else {
		log.Println(task.seg.downloadCount, task.fileLength)
	}
	task.updateState(st)
	log.Println(task.seg)
	if task.deleteFileWhenStop {
		os.Remove(task.savePath)
		log.Println("delete", task.savePath)
	}
	task.wroteToDiskLock.Lock()
	defer task.wroteToDiskLock.Unlock()
	task.capture(true)
}

// 通知事件给外部
func (task *Task) notifyEvent(event string, data interface{}) {
	task.manager.eventNotify(&DownloadEvent{
		TaskId: task.id,
		Event:  event,
		Data:   data,
	})
}

// 更新任务状态
func (task *Task) updateState(state TaskState) {
	task.state = state
	data := map[string]interface{}{
		"state": state,
	}
	data["progress"] = atomic.LoadInt64(&task.seg.downloadCount)
	if state == ERRORED {
		data["error"] = task.lastErr.Error()
	}
	task.notifyEvent("task.state", data)
}

// 恢复任务
func (task *Task) resume(str string) (err error) {
	if task.state != WaitResume {
		return errors.New("任务当前状态不能resume")
	}
	defer func() {
		if err != nil {
			task.lastErr = errors.Wrap(err, "任务恢复出错")
			task.updateState(ERRORED)
		}
	}()

	panic("implement me")
	go func() {
		err := task.init(true)
		if err != nil {
			task.lastErr = err
			task.updateState(ERRORED)
		} else {
			task.updateState(WaitStart)
		}
	}()
	return nil
}

func (task *Task) handleHttp(writer http.ResponseWriter, request *http.Request) {
	if task.fileLength == 0 {
		writer.WriteHeader(403)
		writer.Write([]byte("task not ready"))
		return
	}
	log.Println(request.Header)
	var err error
	from := 0
	end := int(task.fileLength - 1)
	str := strings.Trim(request.Header.Get("Range"), " ")
	if str != "" {
		log.Println(str)
		regexp.MustCompile(`^bytes=\d+\-(\d+)?$`)
		rangeInvalid := str == "" // 当心编译器优化
	RangeInvalid:
		if rangeInvalid {
			writer.WriteHeader(403)
			writer.Write([]byte("range header invalid " + str))
			return
		}
		rangeInvalid = true

		sp := strings.Split(str, "=")
		if len(sp) != 2 || sp[0] != "bytes" {
			log.Println(sp)
			goto RangeInvalid
		}
		sp = strings.Split(sp[1], "-")
		if len(sp) != 2 {
			log.Println(sp)
			goto RangeInvalid
		}
		from, err = strconv.Atoi(sp[0])
		if err != nil {
			log.Println(err)
			goto RangeInvalid
			return
		}
		if sp[1] != "" {
			end, err = strconv.Atoi(sp[1])
			if err != nil {
				log.Println(err)
				goto RangeInvalid
				return
			}
		}
	}
	log.Println("here")
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Content-Length", fmt.Sprint(end-from+1))
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, end, task.fileLength))
	writer.WriteHeader(206)
	log.Println("here")
	r := &eagerReader{
		task:         task,
		from:         int64(from),
		end:          int64(end),
		workerNumber: 16,
	}
	r.init()
	defer r.Close()
	bin := make([]byte, 1024)
	for {
		n, err := r.Read(bin)
		if n > 0 {
			_, e := writer.Write(bin[:n])
			if e != nil {
				return
			}
		}
		if err != nil {
			log.Println(err, r.next, r.end)
			return
		}
	}
}
