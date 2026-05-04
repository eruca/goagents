// Package tools defines tool specifications, registries, middleware, and
// executors.
//
// A Tool is a host-owned typed action. Keep tools focused on one operation and
// describe their boundary in Spec. Permission declares whether the operation is
// read, write, or exec so policy can approve it. ExecutionMode controls
// scheduling: read tools run in parallel by default, while write, exec, empty,
// and unknown permissions run sequentially unless explicitly marked parallel.
// Schema validates model-supplied JSON before the tool body runs. Timeout
// bounds tool and middleware execution.
//
// Tool results separate model-visible and host-visible output. ForLLM is the
// observation appended to model context. ForUser is available to host and UI
// surfaces. Ref can point to a host-owned artifact, and Metadata can carry small
// structured facts about that artifact. Silent suppresses a successful result
// from model context. IsError marks a recoverable domain error the model can
// correct on a later turn. Return a Go error for executor failures that should
// abort the run.
//
// Missing tools and common executor failures return errors classifiable with
// errors.Is: ErrToolNotFound, ErrToolInputInvalid, ErrToolSchemaInvalid,
// ErrToolExecutionFailed, and ErrToolTimeout.
package tools
