// Copyright (C) 2024 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dagu-org/dagu/internal/digraph"
	"github.com/dagu-org/dagu/internal/digraph/scheduler"
	"github.com/dagu-org/dagu/internal/persistence/model"
	"github.com/dagu-org/dagu/internal/test"
	"github.com/dagu-org/dagu/internal/util"
	"github.com/stretchr/testify/require"
)

func TestReporter(t *testing.T) {
	for scenario, fn := range map[string]func(
		t *testing.T, rp *reporter, dag *digraph.DAG, nodes []*model.Node,
	){
		"create error mail":   testErrorMail,
		"no error mail":       testNoErrorMail,
		"create success mail": testSuccessMail,
		"create summary":      testRenderSummary,
		"create node list":    testRenderTable,
	} {
		t.Run(scenario, func(t *testing.T) {

			d := &digraph.DAG{
				Name: "test DAG",
				MailOn: &digraph.MailOn{
					Failure: true,
				},
				ErrorMail: &digraph.MailConfig{
					Prefix: "Error: ",
					From:   "from@mailer.com",
					To:     "to@mailer.com",
				},
				InfoMail: &digraph.MailConfig{
					Prefix: "Success: ",
					From:   "from@mailer.com",
					To:     "to@mailer.com",
				},
				Steps: []digraph.Step{
					{
						Name:    "test-step",
						Command: "true",
					},
				},
			}

			nodes := []*model.Node{
				{
					Step: digraph.Step{
						Name:    "test-step",
						Command: "true",
						Args:    []string{"param-x"},
					},
					Status:     scheduler.NodeStatusRunning,
					StartedAt:  util.FormatTime(time.Now()),
					FinishedAt: util.FormatTime(time.Now().Add(time.Minute * 10)),
				},
			}

			rp := &reporter{
				sender: &mockSender{},
				logger: test.NewLogger(),
			}

			fn(t, rp, d, nodes)
		})
	}
}

func testErrorMail(t *testing.T, rp *reporter, dag *digraph.DAG, nodes []*model.Node) {
	dag.MailOn.Failure = true
	dag.MailOn.Success = false

	_ = rp.send(dag, &model.Status{
		Status: scheduler.StatusError,
		Nodes:  nodes,
	}, fmt.Errorf("Error"))

	mock, ok := rp.sender.(*mockSender)
	require.True(t, ok)
	require.Contains(t, mock.subject, "Error")
	require.Contains(t, mock.subject, "test DAG")
	require.Equal(t, 1, mock.count)
}

func testNoErrorMail(t *testing.T, rp *reporter, dag *digraph.DAG, nodes []*model.Node) {
	dag.MailOn.Failure = false
	dag.MailOn.Success = true

	err := rp.send(dag, &model.Status{
		Status: scheduler.StatusError,
		Nodes:  nodes,
	}, nil)
	require.NoError(t, err)

	mock, ok := rp.sender.(*mockSender)
	require.True(t, ok)
	require.Equal(t, 0, mock.count)
}

func testSuccessMail(t *testing.T, rp *reporter, dag *digraph.DAG, nodes []*model.Node) {
	dag.MailOn.Failure = true
	dag.MailOn.Success = true

	err := rp.send(dag, &model.Status{
		Status: scheduler.StatusSuccess,
		Nodes:  nodes,
	}, nil)
	require.NoError(t, err)

	mock, ok := rp.sender.(*mockSender)
	require.True(t, ok)
	require.Contains(t, mock.subject, "Success")
	require.Contains(t, mock.subject, "test DAG")
	require.Equal(t, 1, mock.count)
}

func testRenderSummary(t *testing.T, _ *reporter, dag *digraph.DAG, nodes []*model.Node) {
	status := &model.Status{
		Name:   dag.Name,
		Status: scheduler.StatusError,
		Nodes:  nodes,
	}
	summary := renderSummary(status, errors.New("test error"))
	require.Contains(t, summary, "test error")
	require.Contains(t, summary, dag.Name)
}

func testRenderTable(t *testing.T, _ *reporter, _ *digraph.DAG, nodes []*model.Node) {
	summary := renderTable(nodes)
	require.Contains(t, summary, nodes[0].Step.Name)
	require.Contains(t, summary, nodes[0].Step.Args[0])
}

type mockSender struct {
	from    string
	to      []string
	subject string
	body    string
	count   int
}

func (m *mockSender) Send(from string, to []string, subject, body string, _ []string) error {
	m.count += 1
	m.from = from
	m.to = to
	m.subject = subject
	m.body = body
	return nil
}
