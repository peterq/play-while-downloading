package downloader

import (
	"reflect"
	"sync"
	"sync/atomic"
)

type segmentStatus uint8

var segmentStructSize uintptr = 0

func init() {
	segmentStructSize = reflect.ValueOf(segment{}).Type().Size()
}

const (
	segFree segmentStatus = iota // 未下载
	segWait                      // 有worker领取了此任务, 稍后下载
	segDl                        // 下载中
	segDone                      // 下载完成
)

// 下载片段
type segment struct {
	status segmentStatus
}

type segmentsInfo struct {
	maxConsecutiveSeg int
	segSize         int64 // 段大小

	slice         []segment
	lock          sync.Mutex
	cond          sync.Cond
	lastSegSize   int64 // 最后一段大小
	initialized   bool
	lastSeg       int64
	downloadCount int64
}

// 初始化片段
func (s *segmentsInfo) init(segSize, totalSize int64, maxConsecutiveSeg int) {
	if s.initialized {
		return
	}
	s.initialized = true
	s.cond.L = &s.lock
	length := totalSize / segSize
	s.lastSegSize = s.segSize
	if totalSize%segSize != 0 {
		s.lastSegSize = totalSize % segSize
		length += 1
	}
	s.slice = make([]segment, length)
	s.segSize = segSize
	s.maxConsecutiveSeg = maxConsecutiveSeg
	s.lastSeg = totalSize - s.lastSegSize
}

// 获取一段下载任务
func (s *segmentsInfo) dl() (start, size int64, num int) {
	s.lock.Lock()
	defer s.lock.Unlock()
	max := s.maxConsecutiveSeg
	size = s.segSize
	for idx, item := range s.slice {
		if item.status == segFree {
			start = int64(idx) * s.segSize
			s.slice[idx].status = segDl
			num = 1
			for num < max && idx < len(s.slice)-1 {
				idx++
				if s.slice[idx].status != segFree {
					return
				} else {
					s.slice[idx].status = segWait
					num++
				}
			}
			return
		}
	}
	return
}

// 优先下载某段
func (s *segmentsInfo) doneOrDl(start int64) (needDownload bool) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if start%s.segSize != 0 {
		panic("not a start of a seg")
	}
	idx := start / s.segSize
	for s.slice[idx].status == segDl { // 如果在下载中则等待
		s.cond.Wait()
	}
	status := s.slice[idx].status
	if status == segWait || status == segFree {
		s.slice[idx].status = segDl
		needDownload = true
	}
	return
}

// 修改某段状态, 失败调用
func (s *segmentsInfo) dlFail(start int64, number int) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if start%s.segSize != 0 {
		panic("not a start of a seg")
	}
	idx := start / s.segSize
	if s.slice[idx].status != segDl {
		return
	}
	s.slice[idx].status = segFree
	number--
	for number > 0 {
		number--
		idx++
		if s.slice[idx].status == segWait {
			s.slice[idx].status = segFree
		}
	}
	s.cond.Broadcast()
	return
}

// 修改某段状态, 成功调用
func (s *segmentsInfo) dlSuccess(start int64) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if start%s.segSize != 0 {
		panic("not a start of a seg")
	}
	idx := start / s.segSize
	if s.slice[idx].status != segDl {
		return
	}
	s.slice[idx].status = segDone
	s.cond.Broadcast()
	added := s.segSize
	if start == s.lastSeg {
		added = s.lastSegSize
	}
	atomic.AddInt64(&s.downloadCount, added)
	return
}

// 等待中变为下载中, 后面还有number段
func (s *segmentsInfo) canContinue(start int64, number int) bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	if start%s.segSize != 0 {
		panic("not a start of a seg")
	}
	idx := start / s.segSize
	old := s.slice[idx].status
	if old == segWait { // 正常情况
		s.slice[idx].status = segDl
		return true
	} else { // 被优先下载了, 不能继续下载, 并且把剩下的重置
		number--
		for number > 0 {
			number--
			idx++
			if s.slice[idx].status == segWait {
				s.slice[idx].status = segFree
			}
		}
		return false
	}
}

