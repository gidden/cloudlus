package cloudlus

import (
	"io"
	"log"
	"os"
	"time"

	"github.com/rwcarlsen/cloudlus/Godeps/_workspace/src/code.google.com/p/go-uuid/uuid"
)

var devnull *os.File

func init() {
	var err error
	devnull, err = os.Open(os.DevNull)
	if err != nil {
		panic(err.Error())
	}
}

type Worker struct {
	Id WorkerId
	// JobTimeout, if nonzero, is a timeout that overrides any timeout
	// specified on each job.
	JobTimeout time.Duration
	ServerAddr string
	FileCache  map[string][]byte
	Wait       time.Duration
	Whitelist  []string
	// lastjob is last time a job was completed.
	lastjob time.Time
	// MaxIdle is the length of time a worker will wait without receiving a
	// job before it shuts itself down.  If MaxIdle is zero, the worker runs
	// forever.
	MaxIdle time.Duration
	nolog   bool
}

func (w *Worker) Run() error {
	uid := uuid.NewRandom()
	copy(w.Id[:], uid)

	w.lastjob = time.Now()
	w.FileCache = map[string][]byte{}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	os.Setenv("PATH", os.Getenv("PATH")+":"+wd)

	if w.Wait == 0 {
		w.Wait = 10 * time.Second
	}

	for {
		wait, err := w.dojob()
		if err != nil {
			log.Print(err)
		}
		if w.MaxIdle > 0 && time.Now().Sub(w.lastjob) > w.MaxIdle {
			log.Printf("no jobs received for %v, shutting down", w.MaxIdle)
			return nil
		}
		if wait {
			<-time.After(w.Wait)
		}
	}
}

func (w *Worker) dojob() (wait bool, err error) {
	client, err := Dial(w.ServerAddr)
	if err != nil {
		return true, err
	}
	defer client.Close()

	j, err := client.Fetch(w)
	if err == nojoberr {
		return false, nil
	} else if err != nil {
		return true, err
	}

	defer func() {
		err2 := client.Push(w, j)
		w.lastjob = time.Now()
		if err == nil && err2 != nil {
			err = err2
		}
	}()

	if w.JobTimeout > 0 {
		j.Timeout = w.JobTimeout
	}

	j.Whitelist(w.Whitelist...)

	// add precached files
	for name, data := range w.FileCache {
		j.AddInfile(name, data)
	}

	// cache new files needing caching
	for _, f := range j.Infiles {
		if f.Cache {
			w.FileCache[f.Name] = f.Data
		}
	}

	done := make(chan struct{})
	defer close(done)
	kill := client.Heartbeat(w.Id, j.Id, done)

	// run job
	if w.nolog {
		j.log = devnull
	}

	pr, pw := io.Pipe()

	rundone := make(chan bool)
	go func() {
		j.Execute(kill, pw)
		pw.Close()
		close(rundone)
	}()

	err = client.PushOutfile(j.Id, pr)
	if err != nil {
		return false, err
	}
	<-rundone
	pr.Close()

	j.WorkerId = w.Id
	j.Infiles = nil // don't need to send back input files

	return false, nil
}
