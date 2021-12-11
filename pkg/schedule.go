package butt

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adhocore/gronx"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type Schedule struct {
	Jobs map[string]*JobSpec `yaml:"jobs" json:"jobs"`
}

func (s *Schedule) Run(surpressLogs bool) {
	gronx := gronx.New()
	ticker := time.NewTicker(time.Second)

	for range ticker.C {
		for _, j := range s.Jobs {
			if j.Cron == "" {
				continue
			}
			due, _ := gronx.IsDue(j.Cron)

			if due {
				go func(j *JobSpec) {
					j.ExecCommandWithRetry("cron", surpressLogs)
				}(j)
			}
		}
	}

}

type JobSpec struct {
	Cron           string      `yaml:"cron,omitempty" json:"cron,omitempty"`
	Command        StringArray `yaml:"command" json:"command"`
	Triggers       []string    `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	Name           string      `json:"name"`
	Retries        int         `yaml:"retries,omitempty" json:"retries,omitempty"`
	globalSchedule *Schedule
	runs           []JobRun
}

type JobRun struct {
	Status      int       `json:"status"`
	Log         string    `json:"log"`
	Name        string    `json:"name"`
	TriggeredAt time.Time `json:"triggered_at"`
	TriggeredBy string    `json:"triggered_by"`
	Triggered   []string  `json:"triggered,omitempty"`
}

func (j *JobSpec) LoadRuns() {
	const nRuns int = 30
	logFn := path.Join(buttPath(), fmt.Sprintf("%s.job.jsonl", j.Name))
	jrs, err := readLastJobRuns(logFn, nRuns)
	if err != nil {
		log.Warn().Str("job", j.Name).Err(err).Msgf("could not load job logs from '%s'", logFn)

	}
	j.runs = jrs

}

func buttPath() string {
	usr, _ := user.Current()
	dir := usr.HomeDir
	p := path.Join(dir, ".butt")
	_ = os.MkdirAll(p, os.ModePerm)

	return p
}

func (j *JobRun) LogToDisk() {
	logFn := path.Join(buttPath(), fmt.Sprintf("%s.job.jsonl", j.Name))
	f, err := os.OpenFile(logFn,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Warn().Str("job", j.Name).Err(err).Msgf("Can't open job log '%v' for writing", logFn)
		return
	}
	defer f.Close()

	if err := json.NewEncoder(f).Encode(j); err != nil {
		log.Warn().Str("job", j.Name).Err(err).Msg("Couldn't save job log to disk.")
	}
}

type StringArray []string

func (a *StringArray) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var multi []string
	err := unmarshal(&multi)
	if err != nil {
		var single string
		err := unmarshal(&single)
		if err != nil {
			return err
		}
		*a = []string{single}
	} else {
		*a = multi
	}
	return nil
}

func readSpecs(fn string) (Schedule, error) {
	yfile, err := ioutil.ReadFile(fn)

	if err != nil {
		log.Error().Err(err)
		return Schedule{}, err
	}

	specs := Schedule{}

	if err = yaml.Unmarshal(yfile, &specs); err != nil {

		log.Error().Err(err)
		return Schedule{}, err
	}

	return specs, nil

}

func loadSchedule(fn string) (Schedule, error) {
	s, err := readSpecs(fn)
	if err != nil {
		return Schedule{}, err
	}

	// run validations
	for k, v := range s.Jobs {
		// validate cron string
		if v.Cron != "" {
			gronx := gronx.New()
			if !gronx.IsValid(v.Cron) {
				return Schedule{}, fmt.Errorf("cron string for job '%s' not valid", k)

			}
		}
		// check if trigger references exist
		for _, t := range v.Triggers {
			if _, ok := s.Jobs[t]; !ok {
				return Schedule{}, fmt.Errorf("cannot find spec of job '%s' that is referenced in job '%s'", t, k)
			}

		}
		// set so metadata / refs to each job struct
		// for easier retrievability
		v.Name = k
		v.globalSchedule = &s
	}

	return s, nil
}

func (j *JobSpec) ExecCommandWithRetry(trigger string, supressLogs bool) {
	tries := 0
	var jr JobRun
	const timeOut = 5 * time.Second

	for tries < j.Retries+1 {

		switch {
		case tries == 0:
			jr = j.ExecCommand(trigger, supressLogs)
		default:
			jr = j.ExecCommand(fmt.Sprintf("%s[retry=%v]", trigger, tries), supressLogs)
		}

		if jr.Status == 0 {
			break
		}
		log.Debug().Str("job", j.Name).Msgf("Job exited unsucessfully, launching retry after %v timeout.", timeOut)
		tries++
		time.Sleep(timeOut)

	}
}

func (j *JobSpec) ExecCommand(trigger string, surpressLogs bool) JobRun {
	log.Info().Str("job", j.Name).Str("trigger", trigger).Msgf("Job triggered")
	// init status to non-zero until execution says otherwise
	jr := JobRun{Name: j.Name, TriggeredAt: time.Now(), TriggeredBy: trigger, Status: -1}
	cmd := exec.Command(j.Command[0], j.Command[1:]...)

	outPipe, _ := cmd.StdoutPipe()
	errPipe, _ := cmd.StderrPipe()

	err := cmd.Start()
	if err != nil {
		jr.Log = fmt.Sprintf("Job unable to start: %v", err.Error())
		if !surpressLogs {
			fmt.Println(err.Error())
		}
		log.Warn().Str("job", j.Name).Err(err).Msgf("Job unable to start")
		jr.LogToDisk()
		return jr
	}

	merged := io.MultiReader(outPipe, errPipe)
	reader := bufio.NewReader(merged)
	line, err := reader.ReadString('\n')

	for err == nil {
		if !surpressLogs {
			fmt.Print(line)
		}
		jr.Log += line
		line, err = reader.ReadString('\n')
	}

	if err := cmd.Wait(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			// j.Statuses = append(j.Statuses, exitError.ExitCode())
			jr.Status = exitError.ExitCode()
			log.Warn().Str("job", j.Name).Msgf("Exit code %v", exitError.ExitCode())
		}
		return jr
	}

	jr.Status = 0
	// trigger jobs that should run on succesful completion
	for _, tn := range j.Triggers {
		tj := j.globalSchedule.Jobs[tn]
		go func(jobName string) {
			tj.ExecCommandWithRetry(fmt.Sprintf("job[%s]", jobName), surpressLogs)
		}(tn)
	}

	jr.LogToDisk()

	return jr

}

func server(s *Schedule, httpPort string) {
	var httpAddr string = fmt.Sprintf(":%s", httpPort)
	type Healthz struct {
		Jobs   int    `json:"jobs"`
		Status string `json:"status"`
	}

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		status := Healthz{Jobs: len(s.Jobs), Status: "ok"}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(status); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

	})

	http.HandleFunc("/schedule", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

	})

	log.Info().Msgf("Starting HTTP server on %v", httpAddr)
	log.Fatal().Err(http.ListenAndServe(httpAddr, nil))

}

func RunSchedule(fn string, prettyLog bool, httpPort string, supressLogs bool, logLevel string) {
	// config logger
	var multi zerolog.LevelWriter

	const logFile string = "core.butt.jsonl"
	logFn := path.Join(buttPath(), logFile)
	f, err := os.OpenFile(logFn,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Can't open log file '%s' for writing.", logFile)
		os.Exit(1)
	}
	defer f.Close()

	if prettyLog {
		multi = zerolog.MultiLevelWriter(f, zerolog.ConsoleWriter{Out: os.Stdout})
	} else {
		multi = zerolog.MultiLevelWriter(f, os.Stdout)
	}

	level, err := zerolog.ParseLevel(logLevel)
	if err != nil {
		fmt.Printf("Exiting, cannot initialize logger with level '%s'\n", logLevel)
		os.Exit(1)
	}
	log.Logger = zerolog.New(multi).With().Timestamp().Logger().Level(level)

	js, err := loadSchedule(fn)
	if err != nil {
		log.Error().Err(err).Msg("")
		os.Exit(1)
	}
	numberJobs := len(js.Jobs)
	i := 0
	for _, job := range js.Jobs {
		log.Info().Msgf("Initializing (%v/%v) job: %s", i, numberJobs, job.Name)
		i++
	}
	go server(&js, httpPort)
	js.Run(supressLogs)

}