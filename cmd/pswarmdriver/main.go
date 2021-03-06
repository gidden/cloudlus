package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/rwcarlsen/cloudlus/Godeps/_workspace/src/github.com/rwcarlsen/go-sqlite/sqlite3"
	"github.com/rwcarlsen/cloudlus/Godeps/_workspace/src/github.com/rwcarlsen/optim"
	"github.com/rwcarlsen/cloudlus/Godeps/_workspace/src/github.com/rwcarlsen/optim/pattern"
	"github.com/rwcarlsen/cloudlus/Godeps/_workspace/src/github.com/rwcarlsen/optim/swarm"
	"github.com/rwcarlsen/cloudlus/cloudlus"
	"github.com/rwcarlsen/cloudlus/scen"
)

var (
	scenfile     = flag.String("scen", "scenario.json", "file containing problem scenification")
	addr         = flag.String("addr", "", "address to submit jobs to (otherwise, run locally)")
	npar         = flag.Int("npar", 0, "number of particles (0 => choose automatically)")
	seed         = flag.Int("seed", 1, "seed for random number generator")
	maxeval      = flag.Int("maxeval", 50000, "max number of objective evaluations")
	maxiter      = flag.Int("maxiter", 500, "max number of optimizer iterations")
	pollrandn    = flag.Int("pollrandn", 0, "use `n` random direction polling")
	maxnoimprove = flag.Int("maxnoimprove", 100, "max iterations with no objective improvement(zero -> infinite)")
	timeout      = flag.Duration("timeout", 120*time.Minute, "max time before remote function eval times out")
	objlog       = flag.String("objlog", "obj.log", "file to log unpenalized objective values")
	runlog       = flag.String("runlog", "run.log", "file to log local cyclus run output")
	dbname       = flag.String("db", "pswarm.sqlite", "name for database containing optimizer work")
	restart      = flag.Int("restart", -1, "iteration to restart from (default is no restart)")
)

const outfile = "objective.out"

func init() {
	log.SetFlags(0)
	flag.Usage = func() {
		log.Printf("Usage: pswarmdriver [opts]\n")
		log.Println("Uses a PSwarm-like solver to find optimum solutions for the scenario.")
		flag.PrintDefaults()
	}
}

var db *sql.DB
var client *cloudlus.Client

func main() {
	var err error
	flag.Parse()
	optim.Rand = rand.New(rand.NewSource(int64(*seed)))

	if _, err := os.Stat(*dbname); !os.IsNotExist(err) && *restart < 0 {
		log.Fatalf("db file '%v' already exists", *dbname)
	}

	db, err = sql.Open("sqlite3", *dbname)
	check(err)
	defer db.Close()

	if *addr != "" {
		client, err = cloudlus.Dial(*addr)
		check(err)
		defer client.Close()
	}

	params := make([]int, flag.NArg())
	for i, s := range flag.Args() {
		params[i], err = strconv.Atoi(s)
		check(err)
	}

	// load problem scen file
	scen := &scen.Scenario{}
	err = scen.Load(*scenfile)
	check(err)

	f1, err := os.Create(*objlog)
	check(err)
	defer f1.Close()
	f4, err := os.Create(*runlog)
	check(err)
	defer f4.Close()

	// create and initialize solver
	lb := scen.LowerBounds()
	ub := scen.UpperBounds()

	step := (ub[0] - lb[0]) / 10
	var it optim.Method

	if *restart >= 0 {
		it, step = loadIter(lb, ub, *restart)
	} else {
		it = buildIter(lb, ub)
	}

	obj := &optim.ObjectiveLogger{Obj: &obj{scen, f4}, W: f1}

	m := &optim.BoxMesh{Mesh: &optim.InfMesh{StepSize: step}, Lower: lb, Upper: ub}

	// this is here so that signals goroutine can close over it
	solv := &optim.Solver{
		Method:       it,
		Obj:          obj,
		Mesh:         m,
		MaxIter:      *maxiter,
		MaxEval:      *maxeval,
		MaxNoImprove: *maxnoimprove,
	}

	// handle signals
	start := time.Now()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		f1.Close()
		f4.Close()
		fmt.Println("\n*** optimizer killed early ***")
		final(solv, start)
		os.Exit(1)
	}()

	// solve and print results
	for solv.Next() {
		fmt.Printf("Iter %v (%v evals):  %v\n", solv.Niter(), solv.Neval(), solv.Best())
	}

	final(solv, start)
}

func final(s *optim.Solver, start time.Time) {
	_, err := db.Exec("CREATE TABLE IF NOT EXISTS optiminfo (start INTEGER,end INTEGER,niter INTEGER,neval INTEGER);")
	check(err)
	_, err = db.Exec("INSERT INTO optiminfo VALUES (?,?,?,?);", start, time.Now(), s.Niter(), s.Neval())
	check(err)

	if err := s.Err(); err != nil {
		log.Print(err)
	}

	fmt.Printf("best: %v\n", s.Best())
	fmt.Printf("%v optimizer iterations\n", s.Niter())
	fmt.Printf("%v objective evaluations\n", s.Neval())
}

func buildIter(lb, ub []float64) optim.Method {
	mask := make([]bool, len(ub))
	for i := range mask {
		mask[i] = lb[i] < ub[i]
	}

	n := 30 + 1*len(lb)
	if *npar != 0 {
		n = *npar
	} else if n < 30 {
		n = 30
	}

	fmt.Printf("swarming with %v particles\n", n)

	ev := optim.ParallelEvaler{ContinueOnErr: true}
	if *addr == "" {
		ev.NConcurrent = 8
	}

	pop := swarm.NewPopulationRand(n, lb, ub)
	swarm := swarm.New(
		pop,
		swarm.Evaler(ev),
		swarm.VmaxBounds(lb, ub),
		swarm.DB(db),
	)
	return pattern.New(pop[0].Point,
		pattern.ResetStep(.0001),
		pattern.NsuccessGrow(4),
		pattern.Evaler(ev),
		pattern.PollRandNMask(*pollrandn, mask),
		pattern.SearchMethod(swarm, pattern.Share),
		pattern.DB(db),
	)
}

func loadPoint(query string, args ...interface{}) *optim.Point {
	rows, err := db.Query(query, args...)
	check(err)
	defer rows.Close()
	posmap := map[int]float64{}
	var obj float64
	for rows.Next() {
		var dim int
		var val float64
		err := rows.Scan(&dim, &val, &obj)
		check(err)
		posmap[dim] = val
	}
	check(rows.Err())

	pos := make([]float64, len(posmap))
	for dim, val := range posmap {
		pos[dim] = val
	}
	return &optim.Point{Pos: pos, Val: obj}
}

func loadIter(lb, ub []float64, iter int) (md optim.Method, initstep float64) {

	_, err := db.Exec("CREATE INDEX IF NOT EXISTS points_posid ON points (posid ASC);")
	check(err)

	query := "SELECT pt.dim,pt.val,pi.val FROM points AS pt JOIN patterninfo AS pi ON pi.posid=pt.posid WHERE pi.iter=?;"
	initPoint := loadPoint(query, iter)

	row := db.QueryRow("SELECT step FROM patterninfo WHERE iter=?;", iter)
	err = row.Scan(&initstep)
	check(err)

	mask := make([]bool, len(ub))
	for i := range mask {
		mask[i] = lb[i] < ub[i]
	}

	row = db.QueryRow("SELECT COUNT(*) FROM swarmparticles WHERE iter=?;", iter)
	var npar int
	err = row.Scan(&npar)
	check(err)

	pop := make(swarm.Population, npar)
	for i := 0; i < npar; i++ {
		query := "SELECT pt.dim,pt.val,s.val FROM points AS pt JOIN swarmparticles AS s ON s.posid=pt.posid WHERE s.iter=? AND s.particle=?;"
		pt := loadPoint(query, iter, i)
		query = "SELECT pt.dim,pt.val,s.best FROM points AS pt JOIN swarmparticlesbest AS s ON s.posid=pt.posid WHERE s.iter=? AND s.particle=?;"
		best := loadPoint(query, iter, i)
		query = "SELECT pt.dim,pt.val,0 FROM points AS pt JOIN swarmparticles AS s ON s.velid=pt.posid WHERE s.iter=? AND s.particle=?;"
		vel := loadPoint(query, iter, i)
		par := &swarm.Particle{
			Id:    i,
			Point: pt,
			Best:  best,
			Vel:   vel.Pos,
		}
		pop[i] = par
		//fmt.Printf("DEBUG par %v: pos[10]=%v obj=%v bestpos[10]=%v bestobj=%v\n", i, par.Pos[10], par.Val, par.Best.Pos[10], par.Best.Val)
	}

	fmt.Printf("swarming with %v particles\n", len(pop))

	ev := optim.ParallelEvaler{ContinueOnErr: true}
	if *addr == "" {
		ev.NConcurrent = runtime.NumCPU()
	}

	swarm := swarm.New(
		pop,
		swarm.Evaler(ev),
		swarm.VmaxBounds(lb, ub),
		swarm.DB(db),
		swarm.InitIter(iter+1),
	)
	return pattern.New(initPoint,
		pattern.ResetStep(.0001),
		pattern.NsuccessGrow(4),
		pattern.Evaler(ev),
		pattern.PollRandNMask(*pollrandn, mask),
		pattern.SearchMethod(swarm, pattern.Share),
		pattern.DB(db),
	), initstep
}

type obj struct {
	s      *scen.Scenario
	runlog io.Writer
}

func (o *obj) Objective(v []float64) (float64, error) {
	scencopyval := *o.s
	scencopy := &scencopyval
	scencopy.TransformVars(v)
	if *addr == "" {
		dbfile, simid, err := scencopy.Run(o.runlog, o.runlog)
		if err != nil {
			return math.Inf(1), err
		}
		defer os.Remove(dbfile)

		return scencopy.CalcObjective(dbfile, simid)
	} else {
		j := buildjob(scencopy)
		return submitjob(scencopy, j)
	}
}

func buildjob(scen *scen.Scenario) *cloudlus.Job {
	scendata, err := json.Marshal(scen)
	check(err)

	tmpldata, err := ioutil.ReadFile(scen.CyclusTmpl)
	check(err)

	j := cloudlus.NewJobCmd("cycdriver", "-obj", "-out", outfile, "-scen", *scenfile)
	j.Timeout = *timeout
	j.AddInfile(scen.CyclusTmpl, tmpldata)
	j.AddInfile(*scenfile, scendata)
	j.AddOutfile(outfile)

	if flag.NArg() > 0 {
		j.Note = strings.Join(flag.Args(), " ")
	}

	return j
}

func submitjob(scen *scen.Scenario, j *cloudlus.Job) (float64, error) {
	var err error
	j, err = client.Run(j)
	if err != nil {
		return math.Inf(1), err
	}

	data, err := client.RetrieveOutfileData(j, outfile)
	if err != nil {
		return math.Inf(1), err
	}

	s := fmt.Sprintf("%s", data)
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		log.Printf("job returned invalid objective string '%v'", s)
		return math.Inf(1), nil
	} else {
		return val, nil
	}

	return math.Inf(1), errors.New("job didn't return proper output file")
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
