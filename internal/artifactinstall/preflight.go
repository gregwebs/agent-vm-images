package artifactinstall

import (
	"archive/tar"
	"errors"
	"io"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
)

// PreflightArchive runs the installer's own archive checks over a file that is
// already on disk, without extracting it.
//
// The maintenance writer is not allowed to have a weaker opinion of an archive than
// the build has: a pin stamped `reviewed` is a promise that the bytes were read the
// way the installer reads them. Both callers therefore run this one function, and
// it is the extraction gate minus the extraction — the member inventory, the
// reviewed mode and size, the ELF machine of every payload binary read out of the
// archive itself.
func PreflightArchive(pin artifactpin.Pin, platform artifactpin.Platform,
	archiveFile string) error {
	archive, err := pin.Archive(platform)
	if err != nil {
		return err
	}
	open := archiveOpener(archiveFile, archive.Format, "")
	entries, err := membersOf(open)
	if err != nil {
		return err
	}
	byName, err := preflight(entries, archive.MemberPrefix)
	if err != nil {
		return err
	}
	if err := checkMembers(pin, byName); err != nil {
		return err
	}
	machine, reviewed := artifactpin.ELFMachines[pin.Name][platform]
	if !reviewed {
		return nil
	}
	return checkArchiveELF(open, pin, byName, machine, platform)
}

// checkArchiveELF reads each payload binary's header straight out of the archive,
// so a reviewer needs 20 bytes rather than an extracted 234 MB tree to know which
// machine the build would install.
func checkArchiveELF(open openArchive, pin artifactpin.Pin, byName map[string]member,
	machine uint16, platform artifactpin.Platform) error {
	for _, entry := range pin.Install.Payload {
		header, err := memberHeader(open, byName, entry.Archive.String())
		if err != nil {
			return err
		}
		if err := checkELF(pin.Name, entry.Published.String(), header, machine, platform); err != nil {
			return err
		}
	}
	return nil
}

func memberHeader(open openArchive, byName map[string]member, name string) ([]byte, error) {
	entry, ok := byName[name]
	if !ok {
		return nil, refuse("the archive carries no %s to publish", name)
	}
	stream, err := open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, refuse("the archive carries no %s to publish", name)
		}
		if err != nil {
			return nil, refuse("the archive is unreadable: %s", err)
		}
		canonical, err := canonicalMember(header.Name, entry.isDir)
		if err != nil {
			return nil, err
		}
		if canonical != name {
			continue
		}
		buffer := make([]byte, ELFHeaderBytes)
		if _, err := io.ReadFull(reader, buffer); err != nil {
			return nil, refuse("the archive carries no %s binary to read: %s", name, err)
		}
		return buffer, nil
	}
}
