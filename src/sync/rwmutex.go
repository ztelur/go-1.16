// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// There is a modified copy of this file in runtime/rwmutex.go.
// If you make any changes here, see if you should make them there.

// A RWMutex is a reader/writer mutual exclusion lock.
// The lock can be held by an arbitrary number of readers or a single writer.
// The zero value for a RWMutex is an unlocked mutex.
//
// A RWMutex must not be copied after first use.
//
// If a goroutine holds a RWMutex for reading and another goroutine might
// call Lock, no goroutine should expect to be able to acquire a read lock
// until the initial read lock is released. In particular, this prohibits
// recursive read locking. This is to ensure that the lock eventually becomes
// available; a blocked Lock call excludes new readers from acquiring the
// lock.
type RWMutex struct {
	w           Mutex  // held if there are pending writers 互斥锁解决多个writer的竞争
	writerSem   uint32 // semaphore for writers to wait for completing readers writer信号量
	readerSem   uint32 // semaphore for readers to wait for completing writers reader信号量
	// 没有 writer 竞争或持有锁时，readerCount 和我们正常理解的 reader 的计数是一样的；
	// 但是，如果有 writer 竞争锁或者持有锁时，那么，readerCount 不仅仅承担着 reader 的计数功能，
	// 还能够标识当前是否有 writer 竞争或持有锁，在这种情况下，请求锁的 reader 的处理进入第 4 行，阻塞等待锁的释放。
	readerCount int32  // number of pending readers reader的数量
	readerWait  int32  // number of departing readers writer等待完成的reader的数量
}

const rwmutexMaxReaders = 1 << 30

// Happens-before relationships are indicated to the race detector via:
// - Unlock  -> Lock:  readerSem
// - Unlock  -> RLock: readerSem
// - RUnlock -> Lock:  writerSem
//
// The methods below temporarily disable handling of race synchronization
// events in order to provide the more precise model above to the race
// detector.
//
// For example, atomic.AddInt32 in RLock should not appear to provide
// acquire-release semantics, which would incorrectly synchronize racing
// readers, thus potentially missing races.

// RLock locks rw for reading.
//
// It should not be used for recursive read locking; a blocked Lock
// call excludes new readers from acquiring the lock. See the
// documentation on the RWMutex type.
func (rw *RWMutex) RLock() {
	// race 的作用
	// https://colobu.com/2020/09/07/go-race-detector-false-positive/#:~:text=%E6%88%91%E4%BB%AC%E7%9F%A5%E9%81%93%EF%BC%8Cgo%20race%20detector,%E5%B0%B1%E4%B8%8D%E4%BC%9A%E6%8A%A5%E8%AD%A6%E4%BA%86)%E3%80%82
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// rw.readerCount是负值的时候，意味着此时有writer等待请求锁，因为writer优先级高，所以把后来的reader阻塞休眠
	if atomic.AddInt32(&rw.readerCount, 1) < 0 {
		// A writer is pending, wait for it.
		runtime_SemacquireMutex(&rw.readerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
}

// RUnlock undoes a single RLock call;
// it does not affect other simultaneous readers.
// It is a run-time error if rw is not locked for reading
// on entry to RUnlock.
func (rw *RWMutex) RUnlock() {
	if race.Enabled {
		_ = rw.w.state
		race.ReleaseMerge(unsafe.Pointer(&rw.writerSem))
		race.Disable()
	}
	// 有等待的writer
	if r := atomic.AddInt32(&rw.readerCount, -1); r < 0 {
		// Outlined slow-path to allow the fast-path to be inlined
		rw.rUnlockSlow(r)
	}
	if race.Enabled {
		race.Enable()
	}
}

func (rw *RWMutex) rUnlockSlow(r int32) {
	if r+1 == 0 || r+1 == -rwmutexMaxReaders {
		race.Enable()
		throw("sync: RUnlock of unlocked RWMutex")
	}
	// 最后一个reader了，writer终于有机会获得锁了
	// A writer is pending.
	if atomic.AddInt32(&rw.readerWait, -1) == 0 {
		// The last reader unblocks the writer.
		runtime_Semrelease(&rw.writerSem, false, 1)
	}
}

// Lock locks rw for writing.
// If the lock is already locked for reading or writing,
// Lock blocks until the lock is available.
func (rw *RWMutex) Lock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// First, resolve competition with other writers.
	// 首先解决其他writer竞争问题
	rw.w.Lock()
	// Announce to readers there is a pending writer.
	// 反转readerCount，告诉reader有writer竞争锁
	// 把它从原来的正整数 readerCount(>=0) 修改为负数（readerCount-rwmutexMaxReaders），
	// 让这个字段保持两个含义（既保存了 reader 的数量，又表示当前有 writer）。
	r := atomic.AddInt32(&rw.readerCount, -rwmutexMaxReaders) + rwmutexMaxReaders
	// Wait for active readers.
	// 如果当前有reader持有锁，那么需要等待
	// RWMutex 需要把这个当前 readerCount 赋值给 readerWait 字段保存下来（第 7 行）
	if r != 0 && atomic.AddInt32(&rw.readerWait, r) != 0 {
		runtime_SemacquireMutex(&rw.writerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
}

// Unlock unlocks rw for writing. It is a run-time error if rw is
// not locked for writing on entry to Unlock.
//
// As with Mutexes, a locked RWMutex is not associated with a particular
// goroutine. One goroutine may RLock (Lock) a RWMutex and then
// arrange for another goroutine to RUnlock (Unlock) it.
func (rw *RWMutex) Unlock() {
	if race.Enabled {
		_ = rw.w.state
		race.Release(unsafe.Pointer(&rw.readerSem))
		race.Disable()
	}
	// 增加 rwmutexMaxReaders 这个常数值。告诉reader没有活跃的writer了
	// Announce to readers there is no active writer.
	r := atomic.AddInt32(&rw.readerCount, rwmutexMaxReaders)
	if r >= rwmutexMaxReaders {
		race.Enable()
		throw("sync: Unlock of unlocked RWMutex")
	}
	// 唤醒阻塞的reader们
	// Unblock blocked readers, if any.
	for i := 0; i < int(r); i++ {
		runtime_Semrelease(&rw.readerSem, false, 0)
	}
	// Allow other writers to proceed.
	// 释放内部的互斥锁
	rw.w.Unlock()
	if race.Enabled {
		race.Enable()
	}
}

// RLocker returns a Locker interface that implements
// the Lock and Unlock methods by calling rw.RLock and rw.RUnlock.
func (rw *RWMutex) RLocker() Locker {
	return (*rlocker)(rw)
}

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }
