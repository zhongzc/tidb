// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package trace

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/tikv/minitrace-go"
	"github.com/tikv/minitrace-go/datadog"
	"github.com/tikv/minitrace-go/jaeger"
	"go.uber.org/atomic"
)

var (
	// JaegerAgent to report tracing results.
	JaegerAgent = atomic.NewString("")
	// DatadogAgent to report tracing results.
	DatadogAgent = atomic.NewString("")
	// DashboardAgent to report tracing results.
	DashboardAgent = atomic.NewString("")
	// MaxSpansLength is the maximum length of spans to report for each SQL.
	MaxSpansLength = atomic.NewUint64(2000)
)

// Report tracing results to Jaeger and Datadog.
func Report(handle minitrace.TraceHandle) {
	spans, c := handle.Collect()
	ctx := c.(*Context)

	jaegerAgent := JaegerAgent.Load()
	datadogAgent := DatadogAgent.Load()
	dashboardAgent := DashboardAgent.Load()

	shouldReportToJaeger := len(jaegerAgent) != 0
	shouldReportToDatadog := len(datadogAgent) != 0
	shouldReportToDashboard := len(dashboardAgent) != 0

	if (!shouldReportToJaeger && !shouldReportToDatadog && !shouldReportToDashboard) || !ctx.ShouldReport {
		return
	}

	spans = truncateSpans(spans, MaxSpansLength.Load())
	traceID := handle.TraceId()
	spanSet := miniSpansToPbSpanSet(traceID, spans)

	traceDetail := ctx.TraceDetail
	traceDetail.SpanSets = append(traceDetail.SpanSets, &spanSet)

	spanSetLen, spanLen := lenOfTraceDetail(traceDetail)
	if spanLen == 0 {
		return
	}

	jgTraces := initJGTraces(shouldReportToJaeger, spanSetLen)
	ddSpanList := initDDSpanList(shouldReportToDatadog, spanLen)

	converter := newConverter(jgTraces, ddSpanList)
	converter.convert(traceDetail)

	reporter := newReporter(jaegerAgent, datadogAgent, dashboardAgent)
	reporter.reportToJaeger(jgTraces)
	reporter.reportToDatadog(ddSpanList)
	reporter.reportToDashboard(traceDetail)
}

func truncateSpans(spans []minitrace.Span, maxSpansLen uint64) []minitrace.Span {
	if len(spans) <= int(maxSpansLen) {
		return spans
	}

	sort.Sort(byBeginUnixTimeNs(spans))
	return spans[:maxSpansLen]
}

func lenOfTraceDetail(traceDetail kvrpcpb.TraceDetail) (spanSetLen int, spanLen int) {
	spanSetLen = len(traceDetail.SpanSets)

	for _, set := range traceDetail.SpanSets {
		spanLen += len(set.Spans)
	}

	return
}

func initJGTraces(
	shouldReportToJaeger bool,
	traceDetailSpanSetLen int,
) *[]jaeger.Trace {
	if !shouldReportToJaeger {
		return nil
	}

	jTraces := make([]jaeger.Trace, 0, traceDetailSpanSetLen)
	return &jTraces
}

func initDDSpanList(
	shouldReportToDatadog bool,
	traceDetailSpanLen int,
) *datadog.SpanList {
	if !shouldReportToDatadog {
		return nil
	}

	s := make([]*datadog.Span, 0, traceDetailSpanLen)
	return (*datadog.SpanList)(&s)
}

func miniSpansToPbSpanSet(
	traceID uint64,
	spans []minitrace.Span,
) kvrpcpb.TraceDetail_SpanSet {
	ss := kvrpcpb.TraceDetail_SpanSet{
		NodeType:         kvrpcpb.TraceDetail_TiDB,
		SpanIdPrefix:     0,
		TraceId:          traceID,
		RootParentSpanId: 0,
		Spans:            make([]*kvrpcpb.TraceDetail_Span, 0, len(spans)),
	}

	for _, span := range spans {
		pps := make([]*kvrpcpb.TraceDetail_Span_Property, 0, len(span.Properties))
		for _, property := range span.Properties {
			pps = append(pps, &kvrpcpb.TraceDetail_Span_Property{
				Key:   property.Key,
				Value: property.Value,
			})
		}
		ss.Spans = append(ss.Spans, &kvrpcpb.TraceDetail_Span{
			Id:              span.ID,
			ParentId:        span.ParentID,
			BeginUnixTimeNs: span.BeginUnixTimeNs,
			DurationNs:      span.DurationNs,
			Event:           span.Event,
			Properties:      pps,
		})
	}

	return ss
}

/// Converter

type converter struct {
	jTraces   *[]jaeger.Trace
	dSpanList *datadog.SpanList

	curTraceID          uint64
	curServiceName      string
	curJTrace           *jaeger.Trace
	curSpanIDPrefix     uint32
	curRootParentSpanID uint64

	tagBuf  []kv
	metaBuf map[string]string
}

func newConverter(
	jTraces *[]jaeger.Trace,
	dSpanList *datadog.SpanList,
) converter {
	return converter{
		jTraces:   jTraces,
		dSpanList: dSpanList,
		tagBuf:    make([]kv, 0, 1024),
	}
}

func (c *converter) convert(traceDetail kvrpcpb.TraceDetail) {
	for _, spanSet := range traceDetail.SpanSets {
		c.nextSpanSet(
			spanSet.NodeType,
			len(spanSet.Spans),
			spanSet.TraceId,
			spanSet.SpanIdPrefix,
			spanSet.RootParentSpanId,
		)

		for _, span := range spanSet.Spans {
			c.appendSpan(span)
		}
	}
}

func (c *converter) nextSpanSet(
	nodeType kvrpcpb.TraceDetail_NodeType,
	spanLen int,
	traceID uint64,
	spanIDPrefix uint32,
	rootParentSpanID uint64,
) {
	var serviceName string
	switch nodeType {
	case kvrpcpb.TraceDetail_TiKV:
		serviceName = "TiKV"
	case kvrpcpb.TraceDetail_TiDB:
		serviceName = "TiDB"
	case kvrpcpb.TraceDetail_PD:
		serviceName = "PD"
	default:
		serviceName = "Unknown"
	}

	if c.jTraces != nil {
		*c.jTraces = append(*c.jTraces, jaeger.Trace{
			TraceID:     int64(traceID),
			ServiceName: serviceName,
			Spans:       make([]jaeger.Span, 0, spanLen),
		})
		c.curJTrace = &(*c.jTraces)[len(*c.jTraces)-1]
	}

	c.curTraceID = traceID
	c.curServiceName = serviceName
	c.curSpanIDPrefix = spanIDPrefix
	c.curRootParentSpanID = rootParentSpanID
}

func (c *converter) appendSpan(span *kvrpcpb.TraceDetail_Span) {
	var parentID int64
	if span.ParentId != 0 {
		parentID = int64(c.curSpanIDPrefix)<<32 | int64(span.ParentId)
	} else {
		parentID = int64(c.curRootParentSpanID)
	}

	c.updateProperties(span.Properties)

	if c.curJTrace != nil {
		c.curJTrace.Spans = append(c.curJTrace.Spans, jaeger.Span{
			SpanID:          int64(c.curSpanIDPrefix)<<32 | int64(span.Id),
			ParentID:        parentID,
			StartUnixTimeUs: int64(span.BeginUnixTimeNs / 1000),
			DurationUs:      int64(span.DurationNs / 1000),
			OperationName:   span.Event,
			Tags:            c.tagBuf,
		})
	}

	if c.dSpanList != nil {
		*c.dSpanList = append(*c.dSpanList, &datadog.Span{
			Name:     span.Event,
			Service:  c.curServiceName,
			Start:    int64(span.BeginUnixTimeNs),
			Duration: int64(span.DurationNs),
			Meta:     c.metaBuf,
			SpanID:   uint64(c.curSpanIDPrefix)<<32 | uint64(span.Id),
			TraceID:  c.curTraceID,
			ParentID: uint64(parentID),
		})
	}
}

func (c *converter) updateProperties(properties []*kvrpcpb.TraceDetail_Span_Property) {
	if len(properties) != 0 {
		c.tagBuf = c.tagBuf[len(c.tagBuf):]
		c.metaBuf = make(map[string]string, len(properties))
		for _, property := range properties {
			c.metaBuf[property.Key] = property.Value
			c.tagBuf = append(c.tagBuf, kv{
				Key:   property.Key,
				Value: property.Value,
			})
		}
	}
}

/// Reporter

type reporter struct {
	jaegerAgent    string
	datadogAgent   string
	dashboardAgent string

	buffer   *bytes.Buffer
	traceBuf []jaeger.Trace
}

func newReporter(jaegerAgent, datadogAgent, dashboardAgent string) reporter {
	return reporter{
		jaegerAgent:    jaegerAgent,
		datadogAgent:   datadogAgent,
		dashboardAgent: dashboardAgent,

		buffer: bytes.NewBuffer(make([]byte, 0, 65535)),
	}
}

func (r *reporter) reportToJaeger(jTraces *[]jaeger.Trace) {
	if jTraces != nil {
		for _, trace := range *jTraces {
			r.splitTrace(trace)
			r.reportTraceBuf()
		}
	}
}

func (r *reporter) reportToDatadog(dSpanList *datadog.SpanList) {
	if dSpanList != nil {
		r.buffer.Truncate(0)
		err := datadog.MessagePackEncode(r.buffer, *dSpanList)
		if err != nil {
			return
		}

		err = datadog.Send(r.buffer, r.datadogAgent)
		if err != nil {
			return
		}
	}
}

type body struct {
	TraceID     uint64 `json:"trace_id"`
	TraceDetail []byte `json:"trace_detail"`
}

func (r *reporter) reportToDashboard(detail kvrpcpb.TraceDetail) {
	bs, err := detail.Marshal()
	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", fmt.Sprintf("http://%s/dashboard/api/trace/report", r.dashboardAgent), bytes.NewReader(bs))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}

	_, err = httpClient.Do(req)
	if err != nil {
		return
	}
}

func (r *reporter) splitTrace(trace jaeger.Trace) {
	if len(trace.Spans) == 0 {
		return
	}
	r.traceBuf = r.traceBuf[:0]

	LIMIT := 50
	totalTraces := (len(trace.Spans) + LIMIT - 1) / LIMIT
	for i := 0; i < totalTraces; i++ {
		r.traceBuf = append(r.traceBuf, jaeger.Trace{
			TraceID:     trace.TraceID,
			ServiceName: trace.ServiceName,
			Spans:       trace.Spans[i*LIMIT : min((i+1)*LIMIT, len(trace.Spans))],
		})
	}

	return
}

func (r *reporter) reportTraceBuf() {
	for _, trace := range r.traceBuf {
		r.buffer.Reset()
		err := jaeger.ThriftCompactEncode(r.buffer, trace)
		if err != nil {
			break
		}

		err = jaeger.Send(r.buffer.Bytes(), r.jaegerAgent)
		if err != nil {
			break
		}
	}
}

/// Helper

type byBeginUnixTimeNs []minitrace.Span

func (a byBeginUnixTimeNs) Len() int           { return len(a) }
func (a byBeginUnixTimeNs) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byBeginUnixTimeNs) Less(i, j int) bool { return a[i].BeginUnixTimeNs < a[j].BeginUnixTimeNs }

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

type kv = struct {
	Key   string
	Value string
}
