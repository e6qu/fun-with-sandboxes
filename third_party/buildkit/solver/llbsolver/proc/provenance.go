package proc

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/executor/resources"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/exporter/containerimage/exptypes"
	gatewaypb "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/frontend/gateway/pb"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/llbsolver"
	provenancetypes "github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/llbsolver/provenance/types"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/solver/result"
	"github.com/e6qu/fun-with-sandboxes/third_party/buildkit/util/tracing"
	"github.com/pkg/errors"
)

func ProvenanceProcessor(slsaVersion provenancetypes.ProvenanceSLSA, attrs map[string]string, customEnv map[string]any) llbsolver.Processor {
	return func(ctx context.Context, res *llbsolver.Result, s *llbsolver.Solver, j *solver.Job, usage *resources.SysSampler) (*llbsolver.Result, error) {
		span, ctx := tracing.StartSpan(ctx, "create provenance attestation")
		defer span.End()

		ps, err := exptypes.ParsePlatforms(res.Metadata)
		if err != nil {
			return nil, err
		}

		var inlineOnly bool
		if v, err := strconv.ParseBool(attrs["inline-only"]); v && err == nil {
			inlineOnly = true
		}

		for _, p := range ps.Platforms {
			cp, ok := res.Provenance.FindRef(p.ID)
			if !ok {
				return nil, errors.Errorf("no build info found for provenance %s", p.ID)
			}

			if cp == nil {
				continue
			}

			ref, ok := res.FindRef(p.ID)
			if !ok {
				return nil, errors.Errorf("could not find ref %s", p.ID)
			}

			pc, err := llbsolver.NewProvenanceCreator(ctx, slsaVersion, cp, ref, attrs, j, usage, customEnv)
			if err != nil {
				return nil, err
			}

			filename := "provenance.json"
			if v, ok := attrs["filename"]; ok {
				filename = v
			}

			res.AddAttestation(p.ID, llbsolver.Attestation{
				Kind: gatewaypb.AttestationKind_InToto,
				Metadata: map[string][]byte{
					result.AttestationReasonKey:     []byte(result.AttestationReasonProvenance),
					result.AttestationInlineOnlyKey: []byte(strconv.FormatBool(inlineOnly)),
				},
				InToto: result.InTotoAttestation{
					PredicateType: pc.PredicateType(),
				},
				Path: filename,
				ContentFunc: func(ctx context.Context) ([]byte, error) {
					pr, err := pc.Predicate(ctx)
					if err != nil {
						return nil, err
					}
					return json.MarshalIndent(pr, "", "  ")
				},
			})
		}

		return res, nil
	}
}
