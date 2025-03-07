// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package options

import (
	"yunion.io/x/pkg/errors"

	"yunion.io/x/onecloud/pkg/apis/compute"
	common_options "yunion.io/x/onecloud/pkg/cloudcommon/options"
	"yunion.io/x/onecloud/pkg/util/ovsutils"
)

const (
	VPC_PROVIDER_OVN = "ovn"
)

const (
	ErrInvalidVpcProvider = errors.Error("invalid vpc provider")
	ErrInvalidOvnDatabase = errors.Error("invalid ovn database")
)

type VpcAgentOptions struct {
	VpcProvider string `default:"ovn"`

	APISyncIntervalSeconds  int `default:"10"`
	APIRunDelayMilliseconds int `default:"100"`
	APIListBatchSize        int `default:"1024"`

	OvnWorkerCheckInterval int    `default:"180"`
	OvnNorthDatabase       string `help:"address for accessing ovn north database.  Default to local unix socket"`
	OvnUnderlayMtu         int    `help:"mtu of ovn underlay network" default:"1500"`
}

type Options struct {
	common_options.CommonOptions

	VpcAgentOptions
}

func (opts *Options) ValidateThenInit() error {
	switch opts.VpcProvider {
	case compute.VPC_PROVIDER_OVN:
	case "":
		return errors.Wrap(ErrInvalidVpcProvider, "empty")
	default:
		return errors.Wrapf(ErrInvalidVpcProvider, "unknown provider: %s", opts.VpcProvider)
	}

	if opts.APIListBatchSize <= 20 {
		opts.APIListBatchSize = 20
	}

	if opts.OvnWorkerCheckInterval <= 60 {
		opts.OvnWorkerCheckInterval = 60
	}

	if opts.OvnUnderlayMtu <= 576 {
		opts.OvnUnderlayMtu = 576
	}

	if db, err := ovsutils.NormalizeDbHost(opts.OvnNorthDatabase); err != nil {
		return err
	} else {
		opts.OvnNorthDatabase = db
	}
	return nil
}
