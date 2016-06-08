package gunfish

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Sirupsen/logrus"
)

var (
	config, _ = LoadConfig("./test/gunfish_test.toml")
)

type TestResponseHandler struct {
	scoreboard map[string]*int
	wg         *sync.WaitGroup
	hook       string
}

func (tr *TestResponseHandler) Done(token string) {
	tr.wg.Done()
}

func (tr *TestResponseHandler) Countup(name string) {
	*(tr.scoreboard[name])++
}

func (tr TestResponseHandler) OnResponse(req *Request, resp *Response, err error) {
	tr.wg.Add(1)
	if err != nil {
		logrus.Warnf(err.Error())
		if err.Error() == MissingTopic.String() {
			tr.Countup(MissingTopic.String())
		}
		if err.Error() == BadDeviceToken.String() {
			tr.Countup(BadDeviceToken.String())
		}
		if err.Error() == Unregistered.String() {
			tr.Countup(Unregistered.String())
		}
	} else {
		tr.Countup("success")
	}
	tr.Done(req.Token)
}

func (tr TestResponseHandler) HookCmd() string {
	return tr.hook
}

func init() {
	logrus.SetLevel(logrus.WarnLevel)
	config.Apns.Host = MockServer
}

func TestStartAndStopSupervisor(t *testing.T) {
	sup, err := StartSupervisor(&config)
	if err != nil {
		t.Errorf("cannot start supvisor: %s", err.Error())
	}

	sup.Shutdown()

	if _, ok := <-sup.queue; ok == true {
		t.Errorf("not closed channel: %v", sup.queue)
	}

	if _, ok := <-sup.retryq; ok == true {
		t.Errorf("not closed channel: %v", sup.queue)
	}

	if _, ok := <-sup.cmdq; ok == true {
		t.Errorf("not closed channel: %v", sup.queue)
	}
}

func TestEnqueuRequestToSupervisor(t *testing.T) {
	// Prepare
	wg := sync.WaitGroup{}
	score := make(map[string]*int, 4)
	for _, v := range []string{MissingTopic.String(), BadDeviceToken.String(), Unregistered.String(), "success"} {
		x := 0
		score[v] = &x
	}

	etr := TestResponseHandler{
		wg:         &wg,
		scoreboard: score,
		hook:       config.Apns.ErrorHook,
	}
	str := TestResponseHandler{
		wg:         &wg,
		scoreboard: score,
	}
	InitErrorResponseHandler(etr)
	InitSuccessResponseHandler(str)

	sup, err := StartSupervisor(&config)
	if err != nil {
		t.Errorf("cannot start supervisor: %s", err.Error())
	}

	// test success requests
	reqs := repeatRequestData("1122334455667788112233445566778811223344556677881122334455667788", 10)
	for range []int{0, 1, 2, 3, 4, 5, 6} {
		sup.EnqueueClientRequest(&reqs)
	}

	// test error requests
	mreqs := repeatRequestData("missingtopic", 1)
	sup.EnqueueClientRequest(&mreqs)

	ureqs := repeatRequestData("unregistered", 1)
	sup.EnqueueClientRequest(&ureqs)

	breqs := repeatRequestData("baddevicetoken", 1)
	sup.EnqueueClientRequest(&breqs)

	time.Sleep(time.Second * 1)
	wg.Wait()
	sup.Shutdown()

	if *(score[MissingTopic.String()]) != 1 {
		t.Errorf("Expected MissingTopic count is 1 but got %d", *(score[MissingTopic.String()]))
	}
	if *(score[Unregistered.String()]) != 1 {
		t.Errorf("Expected Unregistered count is 1 but got %d", *(score[Unregistered.String()]))
	}
	if *(score[BadDeviceToken.String()]) != 1 {
		t.Errorf("Expected BadDeviceToken count is 1 but got %d", *(score[BadDeviceToken.String()]))
	}
	if *(score["success"]) != 70 {
		t.Errorf("Expected success count is 70 but got %d", *(score["success"]))
	}
}

func repeatRequestData(token string, num int) []Request {
	var reqs []Request
	for i := 0; i < num; i++ {
		// Create request
		aps := &APS{
			Alert: &Alert{
				Title: "test",
				Body:  "message",
			},
			Sound: "default",
		}
		payload := Payload{}
		payload.APS = aps

		req := Request{
			Token:   token,
			Payload: payload,
			Tries:   0,
		}

		reqs = append(reqs, req)
	}
	return reqs
}

func TestSuccessOrFailureInvoke(t *testing.T) {
	// prepare SenderResponse
	token := "invalid token"
	sre := fmt.Errorf(Unregistered.String())
	aps := &APS{
		Alert: Alert{
			Title: "test",
			Body:  "hoge message",
		},
		Badge: 1,
		Sound: "default",
	}
	payload := Payload{}
	payload.APS = aps

	sr := SenderResponse{
		Res: &Response{
			ApnsID:     "apns-id-hoge",
			StatusCode: 410,
		},
		Req: Request{
			Token:   token,
			Payload: payload,
			Tries:   0,
		},
		RespTime: 0.0,
		Err:      sre,
	}
	j, err := json.Marshal(sr)
	if err != nil {
		t.Errorf(err.Error())
	}

	// Succeed to invoke
	src := bytes.NewBuffer(j)
	out, err := invokePipe(`cat`, src)
	if err != nil {
		t.Errorf("result: %s, err: %s", string(out), err.Error())
	}

	// checks Unmarshaled result
	if string(out) == `{}` {
		t.Errorf("output of result is empty: %s", string(out))
	}
	if string(out) != string(j) {
		t.Errorf("Expected result %s but got %s", j, string(out))
	}

	// Failure to invoke
	src = bytes.NewBuffer(j)
	out, err = invokePipe(`expr 1 1`, src)
	if err == nil {
		t.Errorf("Expected failure to invoke command: %s", string(out))
	}

	// tests command including Pipe '|'
	src = bytes.NewBuffer(j)
	out, err = invokePipe(`cat | head -n 10 | tail -n 10`, src)
	if err != nil {
		t.Errorf("result: %s, err: %s", string(out), err.Error())
	}
	if string(out) != string(j) {
		t.Errorf("Expected result '%s' but got %s", j, string(out))
	}

	// Must fail
	src = bytes.NewBuffer(j)
	out, err = invokePipe(`echo 'Failure test'; false`, src)
	if err == nil {
		t.Errorf("result: %s, err: %s", string(out), err.Error())
	}
	if fmt.Sprintf("%s", err.Error()) != `exit status 1` {
		t.Errorf("invalid err message: %s", err.Error())
	}
}