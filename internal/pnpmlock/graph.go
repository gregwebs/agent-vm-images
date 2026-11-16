package pnpmlock

// validateProject is the locked-project entry: the graph, the exact peers and
// the peer-context edges, in the retired order.
func validateProject(project *Project, selection Selection) error {
	if project.mode == readTentative {
		return errorf("tentative graph is not a locked project")
	}
	if err := validateGraph(project, selection); err != nil {
		return err
	}
	if selection.Kind == KindDsh {
		if _, err := checkExactPeers(project); err != nil {
			return err
		}
	}
	return validateContextEdges(project)
}

type snapshotIndex struct {
	byKey  map[string]*Snapshot
	byID   []*Snapshot
	byName map[string][]*Snapshot
}

func indexSnapshots(snapshots []Snapshot) snapshotIndex {
	index := snapshotIndex{byKey: map[string]*Snapshot{}, byID: make([]*Snapshot, len(snapshots)), byName: map[string][]*Snapshot{}}
	for position := range snapshots {
		snapshot := &snapshots[position]
		index.byID[position] = snapshot
		index.byKey[snapshot.ID.Key] = snapshot
		name := snapshot.ID.Package.Key()
		index.byName[name] = append(index.byName[name], snapshot)
	}
	return index
}

// resolveContext finds the snapshot a peer-context element names. An opaque hash
// names nothing resolvable; a bare `name@version` must match exactly one
// snapshot, so an ambiguous graph stops instead of guessing.
func (i snapshotIndex) resolve(context PeerContext, source string) (*Snapshot, error) {
	switch typed := context.(type) {
	case OpaqueContext:
		return nil, nil
	case PeerContextID:
		if snapshot, ok := i.byKey[typed.Key]; ok {
			return snapshot, nil
		}
		matches := i.byName[typed.Key]
		if len(matches) > 1 {
			return nil, errorf("%s: ambiguous peer context %s", source, typed.Key)
		}
		if len(matches) != 1 {
			return nil, errorf("%s: dangling peer context %s", source, typed.Key)
		}
		return matches[0], nil
	default:
		panic("pnpmlock: unhandled peer context")
	}
}

func validateContextEdges(project *Project) error {
	index := indexSnapshots(project.snapshots)
	for _, owner := range project.snapshots {
		for _, context := range owner.ID.Context {
			if _, opaque := context.(OpaqueContext); opaque {
				continue
			}
			provider, err := index.resolve(context, owner.ID.Key)
			if err != nil {
				return err
			}
			name, _ := context.(PeerContextID)
			for _, edge := range owner.Edges {
				if edge.Name != name.Package.Name {
					continue
				}
				if provider.ID.Key != edge.Target {
					return errorf("%s: ambiguous context %s", owner.ID.Key, name.Key)
				}
			}
		}
	}
	return nil
}

func validateGraph(project *Project, selection Selection) error {
	if selection != project.selection {
		return errorf("selection differs from manifest")
	}
	index := indexSnapshots(project.snapshots)
	for _, snapshot := range project.snapshots {
		type pendingContext struct {
			context PeerContext
			owner   *Snapshot
		}
		pending := make([]pendingContext, 0, len(snapshot.ID.Context))
		owner := index.byKey[snapshot.ID.Key]
		for _, context := range snapshot.ID.Context {
			pending = append(pending, pendingContext{context: context, owner: owner})
		}
		for len(pending) > 0 {
			item := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			provider, err := index.resolve(item.context, item.owner.ID.Key)
			if err != nil {
				return err
			}
			if _, opaque := item.context.(OpaqueContext); opaque {
				continue
			}
			nested, _ := item.context.(PeerContextID)
			for _, inner := range nested.Context {
				pending = append(pending, pendingContext{context: inner, owner: provider})
			}
		}
	}
	seen := map[string]bool{}
	pending := make([]Edge, len(project.importer))
	copy(pending, project.importer)
	for len(pending) > 0 {
		edge := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		snapshot, ok := index.byKey[edge.Target]
		if !ok {
			return errorf("dangling snapshot edge: %s", edge.Target)
		}
		if seen[edge.Target] {
			continue
		}
		seen[edge.Target] = true
		pending = append(pending, snapshot.Edges...)
	}
	if len(seen) != len(project.snapshots) {
		return errorf("orphan snapshot")
	}
	archiveIDs := map[string]bool{}
	for _, snapshot := range project.snapshots {
		archiveIDs[snapshot.ID.Package.Key()] = true
	}
	if len(archiveIDs) != len(project.packages) {
		return errorf("orphan archive")
	}
	if selection.Kind == KindDsh {
		for _, pkg := range project.packages {
			if pkg.ID.Name.String() == "pnpm" {
				return errorf("dsh cannot own pnpm")
			}
		}
		for _, snapshot := range project.snapshots {
			if snapshot.ID.Package.Name.String() == "pnpm" {
				return errorf("dsh cannot own pnpm")
			}
		}
		reachable := false
		for _, snapshot := range project.snapshots {
			if snapshot.ID.Package.Name.String() == "@deepseek-ai/dsh-sandbox-local" && seen[snapshot.ID.Key] {
				reachable = true
			}
		}
		if !reachable {
			return errorf("dsh sandbox must be reachable")
		}
	}
	return nil
}
