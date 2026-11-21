package pipe

import (
	"context"

	"github.com/yingo-server/UFOT/server/internal/data/job"
	"github.com/yingo-server/UFOT/server/internal/data/pipe/wire"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// Trunkee is the OPTIONAL job-trigger port (kind 980 JOB_TRIGGER → the data
// layer's jobber). The assembly injects the jobPlane; when nil a 980 frame is
// answered with an explicit "jobs not wired" 981 status instead of silence
// (operators must be able to distinguish "job failed" from "no job plane").
type Trunkee interface {
	TriggerJob(ctx context.Context, name string, dryRun bool) (job.Result, error)
}

// errJobsNotWired is the degraded answer text for 980 without a job plane.
const errJobsNotWired = "job plane not wired"

// onJobTrigger handles kind 980: run one job unit (single-flight inside the
// jobber; a concurrent trigger answers busy) and reply 981 JOB_STATUS. Job
// failures are reported in the 981 payload — a failing job is a job outcome,
// not a frame error (docs §6: 作业失败≠进程失败).
func (p *Pipe) onJobTrigger(ctx context.Context, req Req) {
	var jt wire.JobTrigger
	if err := wire.Unmarshal(req.Payload, &jt); err != nil {
		p.reply(buildErrEnv(req, 400, "bad payload"))
		return
	}
	st := wire.JobStatus{Job: jt.Job, At: nowSec()}
	switch {
	case p.trunk == nil:
		st.State = "not_wired"
		st.Detail = errJobsNotWired
	default:
		res, err := p.trunk.TriggerJob(ctx, jt.Job, jt.DryRun)
		switch {
		case err == nil:
			st.State = "ok"
			st.Detail = res.Detail
		case err == job.ErrRunning:
			st.State = "busy"
			st.Detail = "job already running (single-flight)"
		default:
			st.State = "failed"
			st.Detail = err.Error()
		}
	}
	pl, merr := wire.Marshal(st)
	if merr != nil {
		p.reply(buildErrEnv(req, 500, "marshal job status"))
		return
	}
	p.reply(buildResultEnv(req, uint32(kinds.KindJobStatus), pl))
}
