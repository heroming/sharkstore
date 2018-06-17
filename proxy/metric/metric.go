package metric

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"encoding/json"
	"github.com/gogo/protobuf/proto"
	"model/pkg/statspb"
	"util/log"
	"util/metrics"
)

var GsMetric *Metric

type SlowLog struct {
	maxSlowLogNum uint64
	currentIndex  uint64
	slowLog       []*statspb.SlowLog
}

type Metric struct {
	clusterId  uint64
	host       string
	metricAddr string
	cli        *http.Client
	//proxy level
	proxyMeter *metrics.MetricMeter
	//store level
	storeMeter    *metrics.MetricMeter
	maxSlowLogNum uint64

	lock       sync.Mutex
	slowLogger *SlowLog

	connectCount int64

	startTime time.Time
}

func NewMetric(clusterId uint64, host, addr string, maxSlowLogNum uint64) *Metric {
	cli := &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return net.DialTimeout(network, addr, time.Second)
			},
			ResponseHeaderTimeout: time.Second,
		},
	}
	metric := &Metric{
		clusterId:     clusterId,
		host:          host,
		metricAddr:    addr,
		cli:           cli,
		maxSlowLogNum: maxSlowLogNum,
		slowLogger: &SlowLog{
			maxSlowLogNum: maxSlowLogNum,
			slowLog:       make([]*statspb.SlowLog, 0, maxSlowLogNum),
		},
	}
	metric.proxyMeter = metrics.NewMetricMeter("GS-Proxy", time.Second*60, metric)
	metric.storeMeter = metrics.NewMetricMeter("GS-Store", time.Second*60, metric)
	go metric.Run()
	return metric
}

func (m *Metric) Run() {
	timer := time.NewTicker(time.Minute * 10)
	for {
		select {
		case <-timer.C:
			// slowlog
			m.lock.Lock()
			slowLogger := m.slowLogger
			m.slowLogger = &SlowLog{
				maxSlowLogNum: m.maxSlowLogNum,
				slowLog:       make([]*statspb.SlowLog, 0, m.maxSlowLogNum),
			}
			m.lock.Unlock()
			stats := &statspb.SlowLogStats{}
			if len(slowLogger.slowLog) == 0 {
				continue
			}
			stats.SlowLogs = slowLogger.slowLog
			values := url.Values{}
			values.Set("clusterId", fmt.Sprintf("%d", m.clusterId))
			values.Set("namespace", "GS")
			values.Set("subsystem", m.host)
			_url := fmt.Sprintf(`http://%s/metric/slowlog?%s`, m.metricAddr, values.Encode())
			err := m.SendMetric(_url, stats)
			if err != nil {
				log.Warn("send metric server failed, err[%v]", err)
			}
		}
	}
}

func (m *Metric) ProxyApiMetric(method string, ack bool, delay time.Duration) {
	if m == nil {
		return
	}
	m.proxyMeter.AddApiWithDelay(method, ack, delay)
}

func (m *Metric) StoreApiMetric(method string, ack bool, delay time.Duration) {
	if m == nil {
		return
	}
	m.storeMeter.AddApiWithDelay(method, ack, delay)
}

func (m *Metric) SlowLogMetric(slowLog string, delay time.Duration) {
	if m == nil {
		return
	}
	m.lock.Lock()
	defer m.lock.Unlock()
	slowLogger := m.slowLogger
	index := slowLogger.currentIndex
	if uint64(len(slowLogger.slowLog)) < slowLogger.maxSlowLogNum {
		slowLogger.slowLog = append(slowLogger.slowLog, &statspb.SlowLog{
			SlowLog: slowLog,
			Lats:    delay.Seconds(),
		})
	} else {
		slowLogger.slowLog[index%slowLogger.maxSlowLogNum] = &statspb.SlowLog{
			SlowLog: slowLog,
			Lats:    delay.Seconds(),
		}
	}

	slowLogger.currentIndex++
}

func (m *Metric) AddConnectCount(delta int64) {
	if m == nil {
		return
	}
	atomic.AddInt64(&m.connectCount, delta)
}

func (m *Metric) SendMetric(url string, message proto.Message) error {
	data, err := json.Marshal(message)
	if err != nil {
		return err
	}
	request, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	response, err := m.cli.Do(request)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("response not ok!!!!")
	}
	io.Copy(ioutil.Discard, response.Body)
	response.Body.Close()
	return nil
}

func (m *Metric) Output(output interface{}) {
	if m == nil {
		return
	}
	if apiMetric, ok := output.(*metrics.OpenFalconCustomData); ok {
		log.Debug("custom metric %+v", apiMetric)
	} else if tps, ok := output.(*metrics.TpsStats); ok {
		log.Debug("TpsStats metric %+v", tps)
		// report to metric
		stats := &statspb.ProcessStats{}
		// cpu
		cpu_rate, err := cpuRate()
		if err != nil {
			log.Warn("get cpu rate failed, err %v", err)
			return
		}
		stats.CpuProcRate = cpu_rate
		// mem
		total, used, err := memInfo()
		if err != nil {
			log.Warn("get mem info failed, err %v", err)
			return
		}

		// fd
		fdNum, err := fdInfo()
		if err != nil {
			log.Warn("get fd num failed, err %v", err)
			return
		}
		stats.HandleNum = fdNum

		stats.MemoryTotal = total
		stats.MemoryUsed = used
		count := atomic.LoadInt64(&m.connectCount)
		if count < 0 {
			count = 0
			log.Warn("connect count invalid!!!!, must bug!!!!!!")
		}
		stats.ConnectCount = uint64(count)
		stats.ThreadNum = uint32(runtime.NumGoroutine())
		// tps
		stats.TpStats = &statspb.TpStats{
			Tps: tps.Tps,
			// min　latency ms
			Min: tps.Min,
			// max　latency ms
			Max: tps.Max,
			// avg　latency ms
			Avg:         tps.Avg,
			Tp_50:       tps.Tp_50,
			Tp_90:       tps.Tp_90,
			Tp_99:       tps.Tp_99,
			Tp_999:      tps.Tp_999,
			TotalNumber: tps.TotalNumber,
			ErrNumber:   tps.ErrNumber,
		}
		values := url.Values{}
		values.Set("clusterId", fmt.Sprintf("%d", m.clusterId))
		values.Set("namespace", "GS")
		values.Set("subsystem", m.host)
		_url := fmt.Sprintf(`http://%s/metric/process?%s`, m.metricAddr, values.Encode())
		err = m.SendMetric(_url, stats)
		if err != nil {
			log.Warn("send metric server failed, err[%v]", err)
		}
	}
}

func cpuRate() (float64, error) {
	var rate float64

	c := `ps aux`
	cmd := exec.Command("sh", "-c", c)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return rate, err
	}
	pid := os.Getpid()
	find := false
	for {
		line, err := out.ReadString('\n')
		if err != nil {
			break
		}
		tokens := strings.Split(line, " ")
		ft := make([]string, 0)
		for _, t := range tokens {
			if t != "" && t != "\t" {
				ft = append(ft, t)
			}
		}
		_pid, err := strconv.Atoi(ft[1])
		if err != nil {
			continue
		}
		if pid == _pid {
			rate, err = strconv.ParseFloat(ft[2], 64)
			if err != nil {
				return rate, err
			}
			find = true
			break
		}
	}
	if !find {
		return rate, errors.New("not found process!!!!")
	}
	return rate, nil
}

func fdInfo() (num uint32, err error) {
	var files []os.FileInfo
	files, err = ioutil.ReadDir(fmt.Sprintf(`/proc/%d`, os.Getpid()))
	if err != nil {
		return
	}
	num = uint32(len(files))
	return
}
