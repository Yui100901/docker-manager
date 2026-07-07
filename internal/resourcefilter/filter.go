package resourcefilter

import (
	"regexp"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/volume"
)

var (
	ContainerKeys = []string{"name", "id", "image", "state", "status", "label"}
	ImageKeys     = []string{"id", "image", "repo", "tag", "digest", "label"}
	VolumeKeys    = []string{"name", "driver", "mountpoint", "scope", "label", "option"}
)

func Match(candidates []string, filters []string, keys ...string) bool {
	if len(filters) == 0 {
		return true
	}
	keySet := newKeySet(keys)
	for _, filter := range filters {
		if MatchOne(candidates, filter, keySet) {
			return true
		}
	}
	return false
}

func MatchOne(candidates []string, filter string, keySet map[string]bool) bool {
	key, pattern, keyed := SplitFilter(filter, keySet)
	if strings.TrimSpace(pattern) == "" {
		return false
	}
	for _, candidate := range candidates {
		candidateKey, candidateValue, candidateKeyed := SplitFilter(candidate, keySet)
		if keyed {
			if !candidateKeyed || candidateKey != key {
				continue
			}
			if ValueMatches(pattern, candidateValue) {
				return true
			}
			continue
		}
		if ValueMatches(pattern, candidate) {
			return true
		}
	}
	return false
}

func ContainerCandidates(c container.Summary) []string {
	cleanID := strings.TrimPrefix(c.ID, "sha256:")
	candidates := []string{
		c.ID,
		cleanID,
		"id:" + c.ID,
		"id:" + cleanID,
		c.Image,
		"image:" + c.Image,
		string(c.State),
		"state:" + string(c.State),
		c.Status,
		"status:" + c.Status,
	}
	if short := ShortID(c.ID); short != "" && short != cleanID {
		candidates = append(candidates, short, "id:"+short)
	}
	for _, name := range c.Names {
		name = NormalizeContainerName(name)
		candidates = append(candidates, name, "name:"+name)
	}
	if name := FirstContainerName(c.Names); name != "" {
		candidates = append(candidates, name, "name:"+name)
	}
	appendImageReferenceCandidates(&candidates, "image", c.Image, true)
	if c.ImageID != "" {
		imageID := strings.TrimPrefix(c.ImageID, "sha256:")
		candidates = append(candidates, c.ImageID, imageID, "id:"+imageID)
	}
	appendLabelCandidates(&candidates, c.Labels)
	return Unique(candidates)
}

func ImageCandidates(img image.Summary) []string {
	cleanID := strings.TrimPrefix(img.ID, "sha256:")
	candidates := []string{img.ID, cleanID, "id:" + img.ID, "id:" + cleanID}
	if short := ShortID(img.ID); short != "" && short != cleanID {
		candidates = append(candidates, short, "id:"+short)
	}
	for _, tag := range img.RepoTags {
		candidates = append(candidates, tag, "image:"+tag)
		appendImageReferenceCandidates(&candidates, "repo", tag, false)
		appendImageReferenceCandidates(&candidates, "image", tag, true)
		repo, version := SplitRepoTag(tag)
		if repo != "" {
			candidates = append(candidates, "image:"+repo)
		}
		if version != "" {
			candidates = append(candidates, version, "tag:"+version, "image:"+version)
		}
	}
	for _, digest := range img.RepoDigests {
		candidates = append(candidates, digest, "digest:"+digest, "image:"+digest)
	}
	appendLabelCandidates(&candidates, img.Labels)
	return Unique(candidates)
}

func VolumeCandidates(vol *volume.Volume) []string {
	if vol == nil {
		return nil
	}
	candidates := []string{
		vol.Name,
		"name:" + vol.Name,
		vol.Driver,
		"driver:" + vol.Driver,
		vol.Mountpoint,
		"mountpoint:" + vol.Mountpoint,
		vol.Scope,
		"scope:" + vol.Scope,
	}
	appendLabelCandidates(&candidates, vol.Labels)
	for key, value := range vol.Options {
		candidates = append(candidates, key, "option:"+key)
		if value != "" {
			candidates = append(candidates, value, key+"="+value, "option:"+key+"="+value)
		}
	}
	return Unique(candidates)
}

func SplitFilter(filter string, keySet map[string]bool) (string, string, bool) {
	filter = strings.TrimSpace(filter)
	for _, sep := range []string{":", "="} {
		if idx := strings.Index(filter, sep); idx > 0 {
			key := strings.ToLower(strings.TrimSpace(filter[:idx]))
			if keySet[key] {
				return key, strings.TrimSpace(filter[idx+1:]), true
			}
		}
	}
	return "", filter, false
}

func ValueMatches(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	value = strings.TrimSpace(value)
	if pattern == "" || value == "" {
		return false
	}
	if WildcardMatch(pattern, value) || strings.EqualFold(pattern, value) || strings.HasPrefix(strings.ToLower(value), strings.ToLower(pattern)) {
		return true
	}
	if strings.ContainsAny(pattern, "*?") {
		return WildcardMatch(strings.ToLower(pattern), strings.ToLower(value))
	}
	return false
}

func SplitRepoTag(ref string) (string, string) {
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, ""
}

func Unique(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func NormalizeContainerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func FirstContainerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return NormalizeContainerName(names[0])
}

func ShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func WildcardMatch(pattern, value string) bool {
	re, err := regexp.Compile("^" + wildcardToRegex(pattern) + "$")
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

func newKeySet(keys []string) map[string]bool {
	keySet := map[string]bool{}
	for _, key := range keys {
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			keySet[key] = true
		}
	}
	return keySet
}

func appendImageReferenceCandidates(candidates *[]string, key string, ref string, includeTag bool) {
	if ref == "" {
		return
	}
	repo, tag := SplitRepoTag(ref)
	if repo != "" {
		*candidates = append(*candidates, repo, key+":"+repo)
		if slash := strings.Index(repo, "/"); slash >= 0 && slash < len(repo)-1 {
			*candidates = append(*candidates, repo[slash+1:], key+":"+repo[slash+1:])
		}
		if slash := strings.LastIndex(repo, "/"); slash >= 0 && slash < len(repo)-1 {
			*candidates = append(*candidates, repo[slash+1:], key+":"+repo[slash+1:])
		}
	}
	if includeTag && tag != "" {
		*candidates = append(*candidates, tag, key+":"+tag)
	}
}

func appendLabelCandidates(candidates *[]string, labels map[string]string) {
	for key, value := range labels {
		*candidates = append(*candidates, key, "label:"+key)
		if value != "" {
			*candidates = append(*candidates, value, key+"="+value, "label:"+key+"="+value)
		}
	}
}

func wildcardToRegex(pattern string) string {
	var sb strings.Builder
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteByte('.')
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return sb.String()
}
