package gunfish

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/satori/go.uuid"
)

// Supervisor monitor mutiple http2 clients.
type Supervisor struct {
	queue   chan *[]Request // supervisor's queue that recieves POST requests.
	retryq  chan Request    // enqueues this retry queue when to failed to send notification on the http layer.
	cmdq    chan Command    // enqueues this command queue when to get error response from apns.
	exit    chan struct{}   // exit channel is used to stop the supervisor.
	ticker  *time.Ticker    // ticker checks retry queue that has notifications to resend periodically.
	wgrp    *sync.WaitGroup
	workers []*Worker
}

// Worker sends notification to apns.
type Worker struct {
	ac             *ApnsClient
	queue          chan Request
	respq          chan SenderResponse
	wgrp           *sync.WaitGroup
	sn             int
	id             int
	errorHandler   func(*Request, *Response, error)
	successHandler func(*Request, *Response)
}

// SenderResponse is responses to worker from sender.
type SenderResponse struct {
	Res      *Response `json:"response"`
	RespTime float64   `json:"response_time"`
	Req      Request   `json:"request"`
	Err      error     `json:"error_msg"`
	UID      string    `json:"resp_uid"`
}

// Command has execute command and input stream.
type Command struct {
	command string
	input   []byte
}

// EnqueueClientRequest enqueues request to supervisor's queue from external application service
func (s *Supervisor) EnqueueClientRequest(reqs *[]Request) error {
	logf := logrus.Fields{
		"type":             "supervisor",
		"request_size":     len(*reqs),
		"queue_size":       len(s.queue),
		"retry_queue_size": len(s.retryq),
	}

	select {
	case s.queue <- reqs:
		LogWithFields(logf).Debugf("Enqueued request from provider.")
	default:
		LogWithFields(logf).Warnf("Supervisor's queue is full.")
		return fmt.Errorf("Supervisor's queue is full")
	}

	return nil
}

// EnqueueAPNSRequest send Request for APNS to workers.
func (s *Supervisor) EnqueueAPNSRequest(reqs *[]Request) {
	logf := logrus.Fields{
		"type":             "supervisor",
		"queue_size":       len(s.queue),
		"request_size":     len(*reqs),
		"retry_queue_size": len(s.retryq),
	}

	select {
	case s.queue <- reqs:
		LogWithFields(logf).Debugf("Enqueue superviso request queue")
	}
}

// StartSupervisor starts supervisor
func StartSupervisor(conf *Config) (Supervisor, error) {
	// Calculates each worker queue size to accept requests with a given parameter of requests per sec as flow rate.
	var wqSize int
	tp := ((conf.Provider.RequestQueueSize * int(AverageResponseTime/time.Millisecond)) / 1000) / conf.Apns.SenderNum
	dif := (conf.Apns.RequestPerSec - conf.Provider.RequestQueueSize/tp)
	if dif > 0 {
		wqSize = dif * int(FlowRateInterval/time.Second) / conf.Provider.WorkerNum
	} else {
		wqSize = -1 * dif * int(FlowRateInterval/time.Second) / conf.Provider.WorkerNum
	}

	// Initialize Supervisor
	swgrp := &sync.WaitGroup{}
	s := Supervisor{
		queue:  make(chan *[]Request, conf.Provider.QueueSize),
		retryq: make(chan Request, conf.Provider.RequestQueueSize*conf.Provider.WorkerNum),
		cmdq:   make(chan Command, wqSize*conf.Provider.WorkerNum),
		exit:   make(chan struct{}, 1),
		ticker: time.NewTicker(RetryWaitTime),
		wgrp:   swgrp,
	}
	LogWithFields(logrus.Fields{}).Infof("Retry queue size: %d", cap(s.retryq))
	LogWithFields(logrus.Fields{}).Infof("Queue size: %d", cap(s.queue))

	// Time ticker to retry to send
	go func() {
		for {
			select {
			case <-s.ticker.C:
				// Number of request retry send at once.
				for cnt := 0; cnt < RetryOnceCount; cnt++ {
					select {
					case req := <-s.retryq:
						reqs := &[]Request{req}
						select {
						case s.queue <- reqs:
							LogWithFields(logrus.Fields{"type": "retry", "resend_cnt": req.Tries}).
								Debugf("Enqueue to retry to send notification.")
						default:
							LogWithFields(logrus.Fields{"type": "retry"}).
								Infof("Could not retry to enqueue because the supervisor queue is full.")
						}
					default:
						break
					}
				}
			case <-s.exit:
				s.ticker.Stop()
				return
			}
		}
	}()

	// spawn command
	for i := 0; i < conf.Provider.WorkerNum; i++ {
		s.wgrp.Add(1)
		go func() {
			logf := logrus.Fields{"type": "cmd_worker"}
			for c := range s.cmdq {
				LogWithFields(logf).Debugf("invoking command: %s %s", c.command, string(c.input))
				src := bytes.NewBuffer(c.input)
				out, err := invokePipe(c.command, src)
				if err != nil {
					LogWithFields(logf).Errorf("(%s) %s", err.Error(), string(out))
				} else {
					LogWithFields(logf).Debugf("Success to execute command")
				}
			}
			s.wgrp.Done()
		}()
	}

	// Spawn workers
	var err error
	for i := 0; i < conf.Provider.WorkerNum; i++ {
		c, err := NewConnection(conf.Apns.CertFile, conf.Apns.KeyFile, conf.Apns.SkipInsecure)

		if err != nil {
			LogWithFields(logrus.Fields{
				"type": "supervisor",
			}).Errorf("%s", err.Error())
			break
		} else {
			LogWithFields(logrus.Fields{
				"type":      "worker",
				"worker_id": i,
			}).Infoln("Succeeded to establish new connection.")

			worker := Worker{
				id:    i,
				queue: make(chan Request, wqSize),
				respq: make(chan SenderResponse, wqSize),
				wgrp:  &sync.WaitGroup{},
				sn:    conf.Apns.SenderNum,
				ac: &ApnsClient{
					host:   conf.Apns.Host,
					client: c,
				},
			}
			LogWithFields(logrus.Fields{}).Infof("Response queue size: %d", cap(worker.respq))
			LogWithFields(logrus.Fields{}).Infof("Worker Queue size: %d", cap(worker.queue))

			s.workers = append(s.workers, &worker)
			s.wgrp.Add(1)
			go s.spawnWorker(worker, conf)
			LogWithFields(logrus.Fields{
				"type":      "worker",
				"worker_id": i,
			}).Debugf("Spawned worker-%d.", i)
		}
	}

	if err != nil {
		return Supervisor{}, err
	}
	return s, nil
}

// Shutdown supervisor
func (s *Supervisor) Shutdown() {
	LogWithFields(logrus.Fields{
		"type": "supervisor",
	}).Infoln("Waiting for stopping supervisor...")

	// Waiting for processing notification requests
	zeroCnt := 0
	tryCnt := 0
	for zeroCnt < RestartWaitCount {
		// if 's.counter' is not 0 potentially, here loop should not cancel to wait.
		if len(s.queue)+len(s.cmdq)+len(s.retryq)+s.workersAllQueueLength() > 0 {
			zeroCnt = 0
			tryCnt++
		} else {
			zeroCnt++
			tryCnt = 0
		}

		// force terminate application waiting for over 2 min.
		// RestartWaitCount: 50
		// ShutdownWaitTime: 10 (msec)
		// 40 * 50 * 6 * 10 (msec) / 1,000 / 60 = 2 (min)
		if tryCnt > RestartWaitCount*40*6 {
			break
		}

		time.Sleep(ShutdownWaitTime)
	}
	close(s.exit)
	close(s.cmdq)
	s.wgrp.Wait()
	close(s.queue)
	close(s.retryq)

	LogWithFields(logrus.Fields{
		"type": "supervisor",
	}).Infoln("Stoped supervisor.")
}

func (s *Supervisor) spawnWorker(w Worker, conf *Config) {
	atomic.AddInt64(&(srvStats.Workers), 1)
	defer func() {
		atomic.AddInt64(&(srvStats.Workers), -1)
		close(w.respq)
		s.wgrp.Done()
	}()

	// Queue of SenderResopnse
	for i := 0; i < w.sn; i++ {
		w.wgrp.Add(1)
		LogWithFields(logrus.Fields{
			"type":      "worker",
			"worker_id": w.id,
		}).Debugf("Spawned a sender-%d-%d.", w.id, i)

		// spawnSender
		go spawnSender(w.queue, w.respq, w.wgrp, w.ac)
	}

	func() {
		for {
			select {
			case reqs := <-s.queue:
				w.receiveRequests(reqs)
			case resp := <-w.respq:
				w.receiveResponse(resp, s.retryq, s.cmdq)
			case <-s.exit:
				return
			}
		}
	}()

	close(w.queue)
	w.wgrp.Wait()
}

func (w *Worker) receiveResponse(resp SenderResponse, retryq chan<- Request, cmdq chan Command) {
	// initialize logrus fields
	logf := logrus.Fields{
		"type":           "worker",
		"status":         "-",
		"apns_id":        "-",
		"token":          resp.Req.Token,
		"payload":        resp.Req.Payload,
		"worker_id":      w.id,
		"res_queue_size": len(w.respq),
		"resend_cnt":     resp.Req.Tries,
		"response_time":  resp.RespTime,
		"resp_uid":       resp.UID,
	}

	// Response handling
	if resp.Err != nil {
		atomic.AddInt64(&(srvStats.ErrCount), 1)
		if resp.Res != nil {
			logf["status"] = fmt.Sprint(resp.Res.StatusCode)
			logf["apns_id"] = resp.Res.ApnsID

			LogWithFields(logf).Errorf("%s", resp.Err)
		} else {
			// if 'res' is nil,  HTTP connection error with APNS.
			LogWithFields(logf).Warnf("response is nil. reason: %s", resp.Err.Error())

			if resp.Req.Tries < SendRetryCount {
				resp.Req.Tries++
				atomic.AddInt64(&(srvStats.RetryCount), 1)
				logf["resend_cnt"] = resp.Req.Tries

				select {
				case retryq <- resp.Req:
					LogWithFields(logf).
						Debugf("Retry to enqueue into retryq because of http connection error with APNS.")
				default:
					LogWithFields(logf).
						Warnf("Supervisor retry queue is full.")
				}
			} else {
				LogWithFields(logf).
					Warnf("Retry count is over than %d. Could not deliver notification.", SendRetryCount)
			}
		}
		// Error handling
		onResponse(resp, errorResponseHandler.HookCmd(), cmdq)
	} else {
		atomic.AddInt64(&(srvStats.SentCount), 1)
		logf["status"] = fmt.Sprint(resp.Res.StatusCode)
		logf["apns_id"] = resp.Res.ApnsID

		LogWithFields(logf).Info("Succeeded to send a notification")

		// Success handling
		onResponse(resp, "", cmdq) // success response handling does not execute hook command
	}
}

func (w *Worker) receiveRequests(reqs *[]Request) {
	logf := logrus.Fields{
		"type":              "worker",
		"worker_id":         w.id,
		"worker_queue_size": len(w.queue),
		"request_size":      len(*reqs),
	}

	for _, req := range *reqs {
		select {
		case w.queue <- req:
			LogWithFields(logf).
				Debugf("Enqueue request into worker's queue")
		}
	}
}

func spawnSender(wq <-chan Request, respq chan<- SenderResponse, wgrp *sync.WaitGroup, ac *ApnsClient) {
	defer wgrp.Done()
	atomic.AddInt64(&(srvStats.Senders), 1)
	for req := range wq {
		start := time.Now()
		res, err := ac.SendToApns(req)
		respTime := time.Now().Sub(start).Seconds()

		sres := SenderResponse{
			Res:      res,
			RespTime: respTime,
			Req:      req, // Must copy
			Err:      err,
			UID:      uuid.NewV4().String(),
		}

		select {
		case respq <- sres:
			LogWithFields(logrus.Fields{"type": "sender", "resp_queue_size": len(respq)}).
				Debugf("Enqueue response into respq.")
		default:
			LogWithFields(logrus.Fields{"type": "sender", "resp_queue_size": len(respq)}).
				Warnf("Response queue is full.")
		}
	}
}

func (s Supervisor) workersAllQueueLength() int {
	sum := 0
	for _, w := range s.workers {
		sum += len(w.queue) + len(w.respq)
	}
	return sum
}

func onResponse(resp SenderResponse, cmd string, cmdq chan<- Command) {
	logf := logrus.Fields{
		"type":    "on_response",
		"token":   resp.Req.Token,
		"payload": resp.Req.Payload,
	}

	// on error handler
	if resp.Err != nil {
		errorResponseHandler.OnResponse(&resp.Req, resp.Res, resp.Err)
	} else {
		successResponseHandler.OnResponse(&resp.Req, resp.Res, nil)
	}

	// if resp.Res is nil (that is http layer error), not execute command.
	// command string is empty when to succeed to send notification.
	if cmd == "" || resp.Res == nil {
		return
	}

	jresp, err := json.Marshal(resp)
	if err != nil {
		LogWithFields(logf).Errorf(err.Error())
		return
	}

	command := Command{
		command: cmd,
		input:   jresp,
	}
	select {
	case cmdq <- command:
		LogWithFields(logf).Debugf("Enqueue command: %v", command)
	default:
		LogWithFields(logf).Warnf("Command queue is full, so could not execute commnad: %v", command)
	}
}

func invokePipe(hook string, src io.Reader) ([]byte, error) {
	logf := logrus.Fields{"type": "invoke_pipe"}
	cmd := exec.Command("sh", "-c", hook)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed: %v %s", cmd, err.Error())
	}

	// src copy to cmd.stdin
	_, err = io.Copy(stdin, src)
	if e, ok := err.(*os.PathError); ok && e.Err == syscall.EPIPE {
		LogWithFields(logf).Errorf(e.Error())
	} else if err != nil {
		LogWithFields(logf).Errorf("failed to write STDIN: cmd( %s ), error( %s )", hook, err.Error())
	}
	stdin.Close()

	return cmd.CombinedOutput()
}