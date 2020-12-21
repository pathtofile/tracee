package tracee

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

func (t *Tracee) runEventPipeline(done <-chan struct{}) error {
	var errcList []<-chan error

	// Source pipeline stage.
	rawEventChan, errc, err := t.decodeRawEvent(done)
	if err != nil {
		return err
	}
	errcList = append(errcList, errc)

	processedEventChan, errc, err := t.processRawEvent(done, rawEventChan)
	if err != nil {
		return err
	}
	errcList = append(errcList, errc)

	printEventChan, errc, err := t.prepareEventForPrint(done, processedEventChan)
	if err != nil {
		return err
	}
	errcList = append(errcList, errc)

	errc, err = t.printEvent(done, printEventChan)
	if err != nil {
		return err
	}
	errcList = append(errcList, errc)

	// Pipeline started. Waiting for pipeline to complete
	return t.WaitForPipeline(errcList...)
}

type RawEvent struct {
	Ctx      context
	RawArgs  map[argTag]interface{}
	ArgsTags []argTag
}

func (t *Tracee) decodeRawEvent(done <-chan struct{}) (<-chan RawEvent, <-chan error, error) {
	out := make(chan RawEvent)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		for dataRaw := range t.eventsChannel {
			dataBuff := bytes.NewBuffer(dataRaw)
			var ctx context
			err := binary.Read(dataBuff, binary.LittleEndian, &ctx)
			if err != nil {
				errc <- err
				continue
			}

			rawArgs := make(map[argTag]interface{})
			argsTags := make([]argTag, ctx.Argnum)
			for i := 0; i < int(ctx.Argnum); i++ {
				tag, val, err := readArgFromBuff(dataBuff)
				if err != nil {
					errc <- err
					continue
				}
				argsTags[i] = tag
				rawArgs[tag] = val
			}
			select {
			case out <- RawEvent{ctx, rawArgs, argsTags}:
			case <-done:
				return
			}
		}
	}()
	return out, errc, nil
}

func (t *Tracee) processRawEvent(done <-chan struct{}, in <-chan RawEvent) (<-chan RawEvent, <-chan error, error) {
	out := make(chan RawEvent)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		for rawEvent := range in {
			if !t.shouldProcessEvent(rawEvent) {
				continue
			}
			err := t.processEvent(&rawEvent.Ctx, rawEvent.RawArgs)
			if err != nil {
				errc <- err
				continue
			}
			select {
			case out <- rawEvent:
			case <-done:
				return
			}
		}
	}()
	return out, errc, nil
}

func (t *Tracee) getStackTrace(StackID [4]byte) (string, error) {
	stackFrameSize := (strconv.IntSize / 8)
	maxStackDepth := 20

	// Get Map that holds the stack traces
	stackTracesMap, err := t.bpfModule.GetMap("stack_traces")
	if err != nil {
		return "", nil
	}

	// Lookup the StackTraceID in the map
	// The Id could be missing for various reasons, including
	// the id has aged out, or we are not collecting stack traces
	stackBytes, err := stackTracesMap.GetValue(StackID[0:4], stackFrameSize*maxStackDepth)
	if err != nil {
		return "", nil
	}

	var StackTrace string
	for i := 0; i < len(stackBytes); i += stackFrameSize {
		stackAddr := binary.LittleEndian.Uint64(stackBytes[i : i+stackFrameSize])
		if stackAddr == 0 {
			break
		}
		StackTrace += fmt.Sprintf("0x%X,", stackAddr)
	}
	StackTrace = strings.TrimSuffix(StackTrace, ",")
	return StackTrace, nil
}

func (t *Tracee) prepareEventForPrint(done <-chan struct{}, in <-chan RawEvent) (<-chan Event, <-chan error, error) {
	out := make(chan Event, 1000)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errc)
		for rawEvent := range in {
			if !t.shouldPrintEvent(rawEvent) {
				continue
			}
			err := t.prepareArgsForPrint(&rawEvent.Ctx, rawEvent.RawArgs)
			if err != nil {
				errc <- err
				continue
			}
			args := make([]interface{}, rawEvent.Ctx.Argnum)
			argsNames := make([]string, rawEvent.Ctx.Argnum)
			for i, tag := range rawEvent.ArgsTags {
				args[i] = rawEvent.RawArgs[tag]
				argName, ok := t.DecParamName[rawEvent.Ctx.EventID%2][tag]
				if ok {
					argsNames[i] = argName
				} else {
					errc <- fmt.Errorf("Invalid arg tag for event %d", rawEvent.Ctx.EventID)
					continue
				}
			}

			// Add stack trace if needed
			var stackTrace string
			stackTrace, _ = t.getStackTrace(rawEvent.Ctx.StackID)

			evt, err := newEvent(rawEvent.Ctx, argsNames, args, stackTrace)
			if err != nil {
				errc <- err
				continue
			}
			select {
			case out <- evt:
			case <-done:
				return
			}
		}
	}()
	return out, errc, nil
}

func (t *Tracee) printEvent(done <-chan struct{}, in <-chan Event) (<-chan error, error) {
	errc := make(chan error, 1)
	go func() {
		defer close(errc)
		for printEvent := range in {
			t.stats.eventCounter.Increment()
			t.printer.Print(printEvent)
		}
	}()
	return errc, nil
}

// WaitForPipeline waits for results from all error channels.
func (t *Tracee) WaitForPipeline(errs ...<-chan error) error {
	errc := MergeErrors(errs...)
	for err := range errc {
		t.handleError(err)
	}
	return nil
}

// MergeErrors merges multiple channels of errors.
// Based on https://blog.golang.org/pipelines.
func MergeErrors(cs ...<-chan error) <-chan error {
	var wg sync.WaitGroup
	// We must ensure that the output channel has the capacity to hold as many errors
	// as there are error channels. This will ensure that it never blocks, even
	// if WaitForPipeline returns early.
	out := make(chan error, len(cs))

	// Start an output goroutine for each input channel in cs.  output
	// copies values from c to out until c is closed, then calls wg.Done.
	output := func(c <-chan error) {
		for n := range c {
			out <- n
		}
		wg.Done()
	}
	wg.Add(len(cs))
	for _, c := range cs {
		go output(c)
	}

	// Start a goroutine to close out once all the output goroutines are
	// done.  This must start after the wg.Add call.
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
