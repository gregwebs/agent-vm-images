package pnpmlock

import (
	"regexp"
)

var (
	snapshotHead  = regexp.MustCompile(`^(@[^/@()]+/[^@()]+|[^@()/]+)@([^()]+)`)
	opaqueContext = regexp.MustCompile(`^([0-9a-f]{32})\)`)
)

// parseSnapshotID reads pnpm's `name@version(peer@version(...))` syntax. The
// suffix is the dependency context pnpm uses to distinguish two instances of one
// package, so its parse errors are policy stops, not tolerances.
func parseSnapshotID(key string) (SnapshotID, error) {
	offset := 0
	var parse func(depth int) (guardID, error)
	parse = func(depth int) (guardID, error) {
		if depth > maxDepth {
			return guardID{}, errorf("peer context too deep")
		}
		start := offset
		head := snapshotHead.FindStringSubmatch(key[offset:])
		if head == nil {
			return guardID{}, errorf("malformed snapshot: %s", key)
		}
		name, err := ParsePackageName(head[1])
		if err != nil {
			return guardID{}, err
		}
		version, err := ParseExactVersion(head[2])
		if err != nil {
			return guardID{}, err
		}
		id := guardID{packageID: PackageID{Name: name, Version: version}}
		offset += len(head[0])
		seen := map[string]bool{}
		for offset < len(key) && key[offset] == '(' {
			offset++
			if opaque := opaqueContext.FindStringSubmatch(key[offset:]); opaque != nil {
				id.context = append(id.context, OpaqueContext{key: opaque[1]})
				offset += len(opaque[1])
				seen["opaque:"+opaque[1]] = true
			} else {
				nested, err := parse(depth + 1)
				if err != nil {
					return guardID{}, err
				}
				id.context = append(id.context, PeerContextID{Package: nested.packageID, Context: nested.context, Key: nested.key})
				seen["package:"+nested.packageID.Name.String()] = true
			}
			if offset >= len(key) || key[offset] != ')' {
				return guardID{}, errorf("malformed peer suffix: %s", key)
			}
			offset++
		}
		if len(seen) != len(id.context) {
			return guardID{}, errorf("ambiguous peer context: %s", key)
		}
		id.key = key[start:offset]
		return id, nil
	}
	result, err := parse(0)
	if err != nil {
		return SnapshotID{}, err
	}
	if offset != len(key) {
		return SnapshotID{}, errorf("malformed peer suffix: %s", key)
	}
	return SnapshotID{Package: result.packageID, Context: result.context, Key: result.key}, nil
}

type guardID struct {
	packageID PackageID
	context   []PeerContext
	key       string
}
