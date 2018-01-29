// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package time

import (
	"errors"
	"sync"
	"syscall"
)

//go:generate env ZONEINFO=$GOROOT/lib/time/zoneinfo.zip go run genzabbrs.go -output zoneinfo_abbrs_windows.go

// A Location maps time instants to the zone in use at that time.
// 一个 Location 将时间映射到当时使用的区域。
// Typically, the Location represents the collection of time offsets
// 通常，Location 表示时间偏移量的集合
// in use in a geographical area, such as CEST and CET for central Europe.
// 适用于地理区域，如欧洲中部的欧洲中部。

// 此处属性都是从文件中读取的
type Location struct {
	name string
	zone []zone
	tx   []zoneTrans

	// Most lookups will be for the current time
	// 大多数查找会是当前时间。
	// To avoid the binary search through tx, keep a
	// 为了避免在tx中进行二分查找，
	// static one-element cache that gives the correct
	// 保持一个静态的单元素缓存，它在创建位置时提供了正确的时区。
	// zone for the time when the Location was created.
	// if cacheStart <= t < cacheEnd,
	// lookup can return cacheZone.
	// 查询可以返回cacheZone
	// The units for cacheStart and cacheEnd are seconds
	// cacheStart和cacheEnd的单元是自1970年1月1日以来的秒
	// since January 1, 1970 UTC, to match the argument
	// to lookup.
	// 为了匹配查找的参数

	cacheStart int64
	cacheEnd   int64
	cacheZone  *zone		//缓存的时区
}

// A zone represents a single time zone such as CEST or CET.
type zone struct {
	name   string // abbreviated name, "CET"	（时区所惜命）
	offset int    // seconds east of UTC （单位秒）
	isDST  bool   // is this zone Daylight Savings Time? （是否为夏令时）
}

// A zoneTrans represents a single time zone transition.
// 一个zoneTrans代表一个时区的转换
type zoneTrans struct {
	when         int64 // transition time, in seconds since 1970 GMT
											//转换时间，从1970年开始的 秒数
	index        uint8 // the index of the zone that goes into effect at that time
											//在那个时间生效的 时区 的指针
	isstd, isutc bool  // ignored - no idea what these mean
											//忽略-不知道这些是什么意思  （what 黑人问号脸）
}

// alpha and omega are the beginning and end of time for zone
// alpha 和 omega 是时区的开始和结束
// transitions.
const (
	alpha = -1 << 63  // math.MinInt64
	omega = 1<<63 - 1 // math.MaxInt64
)

// UTC represents Universal Coordinated Time (UTC).
// 0 时区
var UTC *Location = &utcLoc

// utcLoc is separate so that get can refer to &utcLoc
// utcLoc是单独的，所以可以引用utcLoc
// and ensure that it never returns a nil *Location
// 确保它不会返回 nil *Location，即使一个错误的行为改变了UTC （只有time中可以使用 nil 作为 0时区）
// even if a badly behaved client has changed UTC.
var utcLoc = Location{name: "UTC"}

// Local represents the system's local time zone.
// 系统当地时间
var Local *Location = &localLoc

// localLoc is separate so that initLocal can initialize
// it even if a client has changed Local.
// 与 zoneinfo_unix.go 初始化
var localLoc Location
var localOnce sync.Once

//获取 Location ，因为 time 中 将 nil 作为了 0时区 的标示
func (l *Location) get() *Location {
	if l == nil {
		return &utcLoc
	}
	if l == &localLoc {
		localOnce.Do(initLocal)
	}
	return l
}

// String returns a descriptive name for the time zone information,
// corresponding to the name argument to LoadLocation or FixedZone.
func (l *Location) String() string {
	return l.get().name
}

// FixedZone returns a Location that always uses
// 直接创建一个 Location
// the given zone name and offset (seconds east of UTC).
// 传递参数为 时区偏移（秒）
func FixedZone(name string, offset int) *Location {
	l := &Location{
		name:       name,
		zone:       []zone{{name, offset, false}},
		tx:         []zoneTrans{{alpha, 0, false, false}},
		cacheStart: alpha,
		cacheEnd:   omega,
	}
	l.cacheZone = &l.zone[0]
	return l
}

// lookup returns information about the time zone in use at an
// instant in time expressed as seconds since January 1, 1970 00:00:00 UTC.
// 查找返回信息为 使用 time 的时区 以秒为单位，从1970年1月1日开始，00:00:00。
//
// The returned information gives the name of the zone (such as "CET"),
// 返回的信息给出了该 时区 的名称(例如“CET”)，
// the start and end times bracketing sec when that zone is in effect,
// 当这一区域生效时，开始和结束时间，
// the offset in seconds east of UTC (such as -5*60*60), and whether
// 时区偏移
// the daylight savings is being observed at that time.
// 以及是否在那时是否为夏令时

//返回了当前时区的信息
func (l *Location) lookup(sec int64) (name string, offset int, isDST bool, start, end int64) {
	l = l.get()

	//zone 长度为 0 则为 零时区
	if len(l.zone) == 0 {
		name = "UTC"
		offset = 0
		isDST = false
		start = alpha
		end = omega
		return
	}

	// 若可以使用缓存，则使用缓存
	if zone := l.cacheZone; zone != nil && l.cacheStart <= sec && sec < l.cacheEnd {
		name = zone.name
		offset = zone.offset
		isDST = zone.isDST
		start = l.cacheStart
		end = l.cacheEnd
		return
	}

  //使用高端算法查找 zone
	if len(l.tx) == 0 || sec < l.tx[0].when {
		zone := &l.zone[l.lookupFirstZone()]
		name = zone.name
		offset = zone.offset
		isDST = zone.isDST
		start = alpha
		if len(l.tx) > 0 {
			end = l.tx[0].when
		} else {
			end = omega
		}
		return
	}

	// Binary search for entry with largest time <= sec.
	//二分搜索zone
	// Not using sort.Search to avoid dependencies.
	// 不使用 sort 库 避免出现依赖
	tx := l.tx
	end = omega
	lo := 0
	hi := len(tx)
	for hi-lo > 1 {
		m := lo + (hi-lo)/2
		lim := tx[m].when
		if sec < lim {
			end = lim
			hi = m
		} else {
			lo = m
		}
	}
	zone := &l.zone[tx[lo].index]
	name = zone.name
	offset = zone.offset
	isDST = zone.isDST
	start = tx[lo].when
	// end = maintained during the search
	return
}


//一言以蔽之 高端算法查找 zone
// lookupFirstZone returns the index of the time zone to use for times
// before the first transition time, or when there are no transition
// times.
//
// The reference implementation in localtime.c from
// http://www.iana.org/time-zones/repository/releases/tzcode2013g.tar.gz
// implements the following algorithm for these cases:
// 1) If the first zone is unused by the transitions, use it.
// 2) Otherwise, if there are transition times, and the first
//    transition is to a zone in daylight time, find the first
//    non-daylight-time zone before and closest to the first transition
//    zone.
// 3) Otherwise, use the first zone that is not daylight time, if
//    there is one.
// 4) Otherwise, use the first zone.
func (l *Location) lookupFirstZone() int {
	// Case 1.
	if !l.firstZoneUsed() {
		return 0
	}

	// Case 2.
	if len(l.tx) > 0 && l.zone[l.tx[0].index].isDST {
		for zi := int(l.tx[0].index) - 1; zi >= 0; zi-- {
			if !l.zone[zi].isDST {
				return zi
			}
		}
	}

	// Case 3.
	for zi := range l.zone {
		if !l.zone[zi].isDST {
			return zi
		}
	}

	// Case 4.
	return 0
}

// firstZoneUsed returns whether the first zone is used by some
// transition.
func (l *Location) firstZoneUsed() bool {
	for _, tx := range l.tx {
		if tx.index == 0 {
			return true
		}
	}
	return false
}

// lookupName returns information about the time zone with
// the given name (such as "EST") at the given pseudo-Unix time
// (what the given time of day would be in UTC).
func (l *Location) lookupName(name string, unix int64) (offset int, ok bool) {
	l = l.get()

	// First try for a zone with the right name that was actually
	// in effect at the given time. (In Sydney, Australia, both standard
	// and daylight-savings time are abbreviated "EST". Using the
	// offset helps us pick the right one for the given time.
	// It's not perfect: during the backward transition we might pick
	// either one.)
	for i := range l.zone {
		zone := &l.zone[i]
		if zone.name == name {
			nam, offset, _, _, _ := l.lookup(unix - int64(zone.offset))
			if nam == zone.name {
				return offset, true
			}
		}
	}

	// Otherwise fall back to an ordinary name match.
	for i := range l.zone {
		zone := &l.zone[i]
		if zone.name == name {
			return zone.offset, true
		}
	}

	// Otherwise, give up.
	return
}

// NOTE(rsc): Eventually we will need to accept the POSIX TZ environment
// syntax too, but I don't feel like implementing it today.

var errLocation = errors.New("time: invalid location name")

var zoneinfo *string
var zoneinfoOnce sync.Once

// LoadLocation returns the Location with the given name.
// 根据给的 时区名 获取 Location
//
// If the name is "" or "UTC", LoadLocation returns UTC.
// If the name is "Local", LoadLocation returns Local.
//
// Otherwise, the name is taken to be a location name corresponding to a file
// in the IANA Time Zone database, such as "America/New_York".
// 时区名是根据文件名设定的，文件名是根据 IANA 时区库设定的
// https://www.iana.org/time-zones
//
// The time zone database needed by LoadLocation may not be
// present on all systems, especially non-Unix systems.
//
// LoadLocation looks in the directory or uncompressed zip file
// named by the ZONEINFO environment variable, if any, then looks in
// known installation locations on Unix systems,
// and finally looks in $GOROOT/lib/time/zoneinfo.zip.

// 加载 Location 所需的时区数据库可能不会出现在所有系统上，尤其是非unix系统。
// LoadLocation 在目录中查找 未压缩的压缩文件 或 命名ZONEINFO环境变量,如果有,那是在在Unix系统上已知的安装位置,
// 最后查找 $GOROOT/lib/time/zoneinfo.zip。

//一次加载，会读写一次文件
func LoadLocation(name string) (*Location, error) {
	if name == "" || name == "UTC" {
		return UTC, nil
	}
	if name == "Local" {
		return Local, nil
	}
	if containsDotDot(name) || name[0] == '/' || name[0] == '\\' {
		// No valid IANA Time Zone name contains a single dot,
		// much less dot dot. Likewise, none begin with a slash.
		return nil, errLocation
	}

	//保证这个方法只调用一次 加锁类似于单例模式
	zoneinfoOnce.Do(func() {
		env, _ := syscall.Getenv("ZONEINFO")
		zoneinfo = &env
	})
	if *zoneinfo != "" {
		if zoneData, err := loadTzinfoFromDirOrZip(*zoneinfo, name); err == nil {
			if z, err := LoadLocationFromTZData(name, zoneData); err == nil {
				return z, nil
			}
		}
	}
	return loadLocation(name, zoneSources)
}

// containsDotDot reports whether s contains "..".
// 判断文件中是否有 .. 
func containsDotDot(s string) bool {
	if len(s) < 2 {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '.' && s[i+1] == '.' {
			return true
		}
	}
	return false
}
