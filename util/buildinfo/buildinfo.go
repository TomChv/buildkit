package buildinfo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"

	"github.com/docker/distribution/reference"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/source"
	binfotypes "github.com/moby/buildkit/util/buildinfo/types"
	"github.com/moby/buildkit/util/urlutil"
	"github.com/pkg/errors"
)

// Decode decodes a base64 encoded build info.
func Decode(enc string) (bi binfotypes.BuildInfo, _ error) {
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return bi, err
	}
	err = json.Unmarshal(dec, &bi)
	return bi, err
}

// Encode encodes build info.
func Encode(ctx context.Context, metadata map[string][]byte, key string, buildSources map[string]string) ([]byte, error) {
	var bi binfotypes.BuildInfo
	if metadata == nil {
		metadata = make(map[string][]byte)
	}
	if v, ok := metadata[key]; ok && v != nil {
		if err := json.Unmarshal(v, &bi); err != nil {
			return nil, err
		}
	}
	if deps, err := decodeDeps(key, bi.Attrs); err == nil {
		bi.Deps = reduceMapBuildInfo(deps, bi.Deps)
	} else {
		return nil, err
	}
	if sources, err := mergeSources(ctx, buildSources, bi.Sources); err == nil {
		bi.Sources = sources
	} else {
		return nil, err
	}
	bi.Attrs = filterAttrs(key, bi.Attrs)
	return json.Marshal(bi)
}

// mergeSources combines and fixes build sources from frontend sources.
func mergeSources(ctx context.Context, buildSources map[string]string, frontendSources []binfotypes.Source) ([]binfotypes.Source, error) {
	// Iterate and combine build sources
	mbs := map[string]binfotypes.Source{}
	for buildSource, pin := range buildSources {
		src, err := source.FromString(buildSource)
		if err != nil {
			return nil, err
		}
		switch sourceID := src.(type) {
		case *source.ImageIdentifier:
			for i, fsrc := range frontendSources {
				// use original user input from frontend sources
				if fsrc.Type == binfotypes.SourceTypeDockerImage && fsrc.Alias == sourceID.Reference.String() {
					if _, ok := mbs[fsrc.Alias]; !ok {
						parsed, err := reference.ParseNormalizedNamed(fsrc.Ref)
						if err != nil {
							return nil, errors.Wrapf(err, "failed to parse %s", fsrc.Ref)
						}
						mbs[fsrc.Alias] = binfotypes.Source{
							Type: binfotypes.SourceTypeDockerImage,
							Ref:  reference.TagNameOnly(parsed).String(),
							Pin:  pin,
						}
						frontendSources = append(frontendSources[:i], frontendSources[i+1:]...)
					}
					break
				}
			}
			if _, ok := mbs[sourceID.Reference.String()]; !ok {
				mbs[sourceID.Reference.String()] = binfotypes.Source{
					Type: binfotypes.SourceTypeDockerImage,
					Ref:  sourceID.Reference.String(),
					Pin:  pin,
				}
			}
		case *source.GitIdentifier:
			sref := sourceID.Remote
			if len(sourceID.Ref) > 0 {
				sref += "#" + sourceID.Ref
			}
			if len(sourceID.Subdir) > 0 {
				sref += ":" + sourceID.Subdir
			}
			if _, ok := mbs[sref]; !ok {
				mbs[sref] = binfotypes.Source{
					Type: binfotypes.SourceTypeGit,
					Ref:  urlutil.RedactCredentials(sref),
					Pin:  pin,
				}
			}
		case *source.HTTPIdentifier:
			if _, ok := mbs[sourceID.URL]; !ok {
				mbs[sourceID.URL] = binfotypes.Source{
					Type: binfotypes.SourceTypeHTTP,
					Ref:  urlutil.RedactCredentials(sourceID.URL),
					Pin:  pin,
				}
			}
		}
	}

	// leftover sources in frontend. Mostly duplicated ones we don't need but
	// there is an edge case if no instruction except sources one is defined
	// (e.g. FROM ...) that can be valid so take it into account.
	for _, fsrc := range frontendSources {
		if fsrc.Type != binfotypes.SourceTypeDockerImage {
			continue
		}
		if _, ok := mbs[fsrc.Alias]; !ok {
			parsed, err := reference.ParseNormalizedNamed(fsrc.Ref)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse %s", fsrc.Ref)
			}
			mbs[fsrc.Alias] = binfotypes.Source{
				Type: binfotypes.SourceTypeDockerImage,
				Ref:  reference.TagNameOnly(parsed).String(),
				Pin:  fsrc.Pin,
			}
		}
	}

	srcs := make([]binfotypes.Source, 0, len(mbs))
	for _, bs := range mbs {
		srcs = append(srcs, bs)
	}
	sort.Slice(srcs, func(i, j int) bool {
		return srcs[i].Ref < srcs[j].Ref
	})

	return srcs, nil
}

// decodeDeps decodes dependencies (buildinfo) added via the input context.
func decodeDeps(key string, attrs map[string]*string) (map[string]binfotypes.BuildInfo, error) {
	var platform string
	// extract platform from metadata key
	skey := strings.SplitN(key, "/", 2)
	if len(skey) == 2 {
		platform = skey[1]
	}

	res := make(map[string]binfotypes.BuildInfo)
	for k, v := range attrs {
		// dependencies are only handled via the input context
		if v == nil || !strings.HasPrefix(k, "input-metadata:") {
			continue
		}

		// if platform is defined, only decode dependencies for that platform
		if platform != "" && !strings.HasSuffix(k, "::"+platform) {
			continue
		}

		// decode input metadata
		var inputresp map[string]string
		if err := json.Unmarshal([]byte(*v), &inputresp); err != nil {
			return nil, errors.Wrap(err, "failed to unmarshal input-metadata")
		}

		// check buildinfo key is present
		if _, ok := inputresp[exptypes.ExporterBuildInfo]; !ok {
			continue
		}

		// decode buildinfo
		bi, err := Decode(inputresp[exptypes.ExporterBuildInfo])
		if err != nil {
			return nil, errors.Wrap(err, "failed to decode buildinfo from input-metadata")
		}

		// set dep key
		var depkey string
		kl := strings.SplitN(k, ":", 2)
		depkey = kl[1]
		if platform != "" {
			depkey = strings.TrimSuffix(depkey, "::"+platform)
		}

		res[depkey] = bi
	}
	if len(res) == 0 {
		return nil, nil
	}
	return res, nil
}

// FormatOpts holds build info format options.
type FormatOpts struct {
	RemoveAttrs bool
}

// Format formats build info.
func Format(dt []byte, format FormatOpts) (_ []byte, err error) {
	if len(dt) == 0 {
		return dt, nil
	}
	var bi binfotypes.BuildInfo
	if err := json.Unmarshal(dt, &bi); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal buildinfo for formatting")
	}
	if format.RemoveAttrs {
		bi.Attrs = nil
	}
	if dt, err = json.Marshal(bi); err != nil {
		return nil, err
	}
	return dt, nil
}

var knownAttrs = []string{
	//"cmdline",
	"context",
	"filename",
	"source",

	//"add-hosts",
	//"cgroup-parent",
	//"force-network-mode",
	//"hostname",
	//"image-resolve-mode",
	//"platform",
	"shm-size",
	"target",
	"ulimit",
}

// filterAttrs filters frontent opt by picking only those that
// could effectively change the build result.
func filterAttrs(key string, attrs map[string]*string) map[string]*string {
	var platform string
	// extract platform from metadata key
	skey := strings.SplitN(key, "/", 2)
	if len(skey) == 2 {
		platform = skey[1]
	}
	filtered := make(map[string]*string)
	for k, v := range attrs {
		if v == nil {
			continue
		}
		// control args are filtered out
		if isControlArg(k) {
			continue
		}
		// always include
		if strings.HasPrefix(k, "build-arg:") || strings.HasPrefix(k, "label:") {
			filtered[k] = v
			continue
		}
		// input context key and value has to be cleaned up
		// before being included
		if strings.HasPrefix(k, "context:") {
			if platform != "" {
				// if platform is defined, only include the relevant platform
				if !strings.HasSuffix(k, "::"+platform) {
					continue
				}
				ctxival := strings.TrimSuffix(*v, "::"+platform)
				filtered[strings.TrimSuffix(k, "::"+platform)] = &ctxival
				continue
			}
			filtered[k] = v
			continue
		}
		// filter only for known attributes
		for _, knownAttr := range knownAttrs {
			if knownAttr == k {
				filtered[k] = v
				break
			}
		}
	}
	return filtered
}

var knownControlArgs = []string{
	"BUILDKIT_CACHE_MOUNT_NS",
	"BUILDKIT_CONTEXT_KEEP_GIT_DIR",
	"BUILDKIT_INLINE_BUILDINFO_ATTRS",
	"BUILDKIT_INLINE_CACHE",
	"BUILDKIT_MULTI_PLATFORM",
	"BUILDKIT_SANDBOX_HOSTNAME",
	"BUILDKIT_SYNTAX",
}

// isControlArg checks if a build attributes is a control arg
func isControlArg(attrKey string) bool {
	for _, k := range knownControlArgs {
		if strings.HasPrefix(attrKey, "build-arg:"+k) {
			return true
		}
	}
	return false
}

// GetMetadata returns buildinfo metadata for the specified key. If the key
// is already there, result will be merged.
func GetMetadata(metadata map[string][]byte, key string, reqFrontend string, reqAttrs map[string]string) ([]byte, error) {
	if metadata == nil {
		metadata = make(map[string][]byte)
	}
	var dtbi []byte
	if v, ok := metadata[key]; ok && v != nil {
		var mbi binfotypes.BuildInfo
		if errm := json.Unmarshal(v, &mbi); errm != nil {
			return nil, errors.Wrapf(errm, "failed to unmarshal build info for %q", key)
		}
		if reqFrontend != "" {
			mbi.Frontend = reqFrontend
		}
		if deps, err := decodeDeps(key, convertMap(reduceMapString(reqAttrs, mbi.Attrs))); err == nil {
			mbi.Deps = reduceMapBuildInfo(deps, mbi.Deps)
		} else {
			return nil, err
		}
		mbi.Attrs = filterAttrs(key, convertMap(reduceMapString(reqAttrs, mbi.Attrs)))
		var err error
		dtbi, err = json.Marshal(mbi)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal build info for %q", key)
		}
	} else {
		deps, err := decodeDeps(key, convertMap(reqAttrs))
		if err != nil {
			return nil, err
		}
		dtbi, err = json.Marshal(binfotypes.BuildInfo{
			Frontend: reqFrontend,
			Attrs:    filterAttrs(key, convertMap(reqAttrs)),
			Deps:     deps,
		})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to marshal build info for %q", key)
		}
	}
	return dtbi, nil
}

// FromImageConfig returns build info from image config.
func FromImageConfig(dt []byte) (*binfotypes.BuildInfo, error) {
	if len(dt) == 0 {
		return nil, nil
	}
	var config binfotypes.ImageConfig
	if err := json.Unmarshal(dt, &config); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal image config")
	}
	if len(config.BuildInfo) == 0 {
		return nil, nil
	}
	bi, err := Decode(config.BuildInfo)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode build info from image config")
	}
	return &bi, nil
}

func reduceMapString(m1 map[string]string, m2 map[string]*string) map[string]string {
	if m1 == nil && m2 == nil {
		return nil
	}
	if m1 == nil {
		m1 = map[string]string{}
	}
	for k, v := range m2 {
		if v != nil {
			m1[k] = *v
		}
	}
	return m1
}

func reduceMapBuildInfo(m1 map[string]binfotypes.BuildInfo, m2 map[string]binfotypes.BuildInfo) map[string]binfotypes.BuildInfo {
	if m1 == nil && m2 == nil {
		return nil
	}
	if m1 == nil {
		m1 = map[string]binfotypes.BuildInfo{}
	}
	for k, v := range m2 {
		m1[k] = v
	}
	return m1
}

func convertMap(m map[string]string) map[string]*string {
	res := make(map[string]*string)
	for k, v := range m {
		value := v
		res[k] = &value
	}
	return res
}
