package filetree

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/anchore/stereoscope/internal"
	"github.com/anchore/stereoscope/pkg/file"
	"github.com/anchore/stereoscope/pkg/filetree/filenode"
	"github.com/anchore/stereoscope/pkg/tree"
	"github.com/anchore/stereoscope/pkg/tree/node"
	"github.com/bmatcuk/doublestar/v2"
)

var ErrRemovingRoot = errors.New("cannot remove the root path (`/`) from the FileTree")
var ErrLinkCycleDetected = errors.New("cycle during symlink resolution")

// FileTree represents a file/directory Tree
type FileTree struct {
	tree *tree.Tree
}

// NewFileTree creates a new FileTree instance.
func NewFileTree() *FileTree {
	t := tree.NewTree()

	// Initialize FileTree with a root "/" node
	_ = t.AddRoot(filenode.NewDir("/", nil))

	return &FileTree{
		tree: t,
	}
}

// Copy returns a Copy of the current FileTree.
func (t *FileTree) Copy() (*FileTree, error) {
	ct := NewFileTree()
	ct.tree = t.tree.Copy()
	return ct, nil
}

// AllFiles returns all files and directories within the FileTree.
func (t *FileTree) AllFiles() []file.Reference {
	var files []file.Reference
	for _, n := range t.tree.Nodes() {
		f := n.(*filenode.FileNode)
		if f.FileType == file.TypeReg && f.Reference != nil {
			files = append(files, *f.Reference)
		}
	}
	return files
}

func (t *FileTree) AllRealPaths() []file.Path {
	var files []file.Path
	for _, n := range t.tree.Nodes() {
		f := n.(*filenode.FileNode)
		if f != nil {
			files = append(files, f.RealPath)
		}
	}
	return files
}

func (t *FileTree) ListPaths(dir file.Path) ([]file.Path, error) {
	_, n, err := t.node(dir, linkResolutionStrategy{
		FollowAncestorLinks: true,
		FollowBasenameLinks: true,
	})
	if err != nil {
		return nil, err
	}

	if n == nil {
		return nil, nil
	}

	if n.FileType != file.TypeDir {
		return nil, nil
	}

	var listing []file.Path
	children := t.tree.Children(n)
	for _, child := range children {
		if child == nil {
			continue
		}
		childFn := child.(*filenode.FileNode)
		basePath, _, err := t.node(childFn.RealPath, linkResolutionStrategy{
			FollowAncestorLinks: true,
			FollowBasenameLinks: false,
		})
		if err != nil {
			return nil, err
		}

		listing = append(listing, file.Path(filepath.Join(string(dir), basePath.Basename())))
	}
	return listing, nil
}

// File fetches a file.Reference for the given path. Returns nil if the path does not exist in the FileTree.
func (t *FileTree) File(path file.Path, options ...LinkResolutionOption) (bool, *file.Reference, error) {
	userStrategy := newLinkResolutionStrategy(options...)
	// For:             /some/path/here
	// Where:           /some/path -> /other/place
	// And resolves to: /other/place/here

	// This means a few things:
	//  - /some/path/here CANNOT exist in the Tree. If it did, the parent /some/path would have to be a directory,
	//      but since we know it is a link this cannot be true.
	//  - /other/place DOES NOT need to exist in the Tree --this would be a dead link and is allowable. Under this case
	//      we return NIL.
	//  - /other/place/here DOES NOT need to exist in the Tree, it either
	//          a) exists as a regular file --in which case return the discovered file.Reference
	//	        b) does not exist --return NIL
	//          c) or exists as a symlink that may or may not resolve --this last case does not matter since the
	//             PATH has been resolved to a file.Reference, which is all that matters)
	//
	// Therefore we can safely lookup the path first without worrying about symlink resolution yet... if there is a
	// hit, return it! If not, fallback to symlink resolution.

	_, currentNode, err := t.node(path, linkResolutionStrategy{})
	if err != nil {
		return false, nil, err
	}
	if currentNode != nil && (!currentNode.IsLink() || currentNode.IsLink() && !userStrategy.FollowBasenameLinks) {
		return true, currentNode.Reference, nil
	}

	// symlink resolution!... within the context of container images (which is outside of the responsibility of this object)
	// the only really valid resolution of symlinks is in squash trees (both for an image and a layer --NOT for trees
	// that represent a single union FS layer.
	_, currentNode, err = t.node(path, linkResolutionStrategy{
		FollowAncestorLinks:          true,
		FollowBasenameLinks:          userStrategy.FollowBasenameLinks,
		DoNotFollowDeadBasenameLinks: userStrategy.DoNotFollowDeadBasenameLinks,
	})
	if currentNode != nil {
		return true, currentNode.Reference, err
	}
	return false, nil, err
}

func (t *FileTree) node(p file.Path, strategy linkResolutionStrategy) (file.Path, *filenode.FileNode, error) {
	if !strategy.FollowLinks() {
		n := t.tree.Node(filenode.IDByPath(p))
		if n == nil {
			return p, nil, nil
		}
		return p, n.(*filenode.FileNode), nil
	}

	var currentNode *filenode.FileNode
	var currentPath = p
	var err error
	if strategy.FollowAncestorLinks {
		currentPath, currentNode, err = t.resolveAncestorLinks(currentPath)
		if err != nil {
			return currentPath, currentNode, err
		}
	} else {
		n := t.tree.Node(filenode.IDByPath(currentPath))
		if n != nil {
			currentNode = n.(*filenode.FileNode)
		}
	}

	// link resolution has come up with nothing, return what we have so far
	if currentNode == nil {
		return currentPath, currentNode, nil
	}

	if strategy.FollowBasenameLinks {
		currentPath, currentNode, err = t.resolveNodeLinks(currentNode, !strategy.DoNotFollowDeadBasenameLinks)
	}
	return currentPath, currentNode, err
}

// return FileNode of the basename in the given path (no resolution is done at or past the basename).
func (t *FileTree) resolveAncestorLinks(path file.Path) (file.Path, *filenode.FileNode, error) {
	var pathParts = strings.Split(string(path.Normalize()), file.DirSeparator)
	var currentPathStr string
	var currentPath file.Path
	var currentNode *filenode.FileNode
	var err error

	// iterate through all parts of the path, replacing path elements with link resolutions where possible.
	for idx, part := range pathParts {
		if (part == "" || part == file.DirSeparator) && idx == 0 {
			// note: this means that we will NEVER resolve a symlink or file.Reference for /, which is OK
			continue
		}

		// cumulatively gather where we are currently at and provide a rich object
		currentPath = file.Path(currentPathStr + file.DirSeparator + part).Normalize()
		currentPathStr = string(currentPath)

		// fetch the node with NO link resolution strategy
		currentPath, currentNode, err = t.node(currentPath, linkResolutionStrategy{})
		if err != nil {
			// should never occur
			return "", nil, err
		}

		if currentNode == nil {
			// we've reached a point where the given path that has never been observed. This can happen for one reason:
			// 1. the current path is really invalid and we should return NIL indicating that it cannot be resolved.
			// 2. the current path is a link? no, this isn't possible since we are iterating through constituent paths
			//      in order, so we are guaranteed to hit parent links in which we should adjust the search path accordingly.
			return currentPath, nil, nil
		}

		// this is positively a path, however, there is no information about this node. This may be OK since we
		// allow for adding children before parents (and even don't require the parent to ever be added --which is
		// potentially valid given the underlying messy data [tar headers]). In this case we keep building the path
		// (which we've already done at this point) and continue.
		if currentNode.Reference == nil {
			continue
		}

		// by this point we definitely have a file reference, if this is a link (and not the basename) resolve any
		// links until the next node is resolved (or not).
		isLastPart := idx == len(pathParts)-1
		if !isLastPart && currentNode.IsLink() {
			currentPath, currentNode, err = t.resolveNodeLinks(currentNode, true)
			if err != nil {
				// only expected to happen on cycles
				return currentPath, currentNode, err
			}
			currentPathStr = string(currentPath)
		}
	}
	// by this point we have processed all constituent paths; there were no un-added paths and the path is guaranteed
	// to have followed link resolution.
	return currentPath, currentNode, nil
}

// followNode takes the given FileNode and resolves all links at the base of the real path for the node (this implies
// that NO ancestors are considered).
func (t *FileTree) resolveNodeLinks(n *filenode.FileNode, followDeadBasenameLinks bool) (file.Path, *filenode.FileNode, error) {
	// TODO nil check?

	// note: this assumes that callers are passing paths in which the constituent parts are NOT symlinks
	var lastNode *filenode.FileNode
	var lastPath file.Path

	currentNode := n
	currentPath := n.RealPath

	// keep resolving links until a regular file or directory is found
	alreadySeen := internal.NewStringSet()
	var err error
	for {
		exists := currentNode != nil

		// if there is no next path, return this reference (dead link)
		if !exists {
			break
		}

		if alreadySeen.Contains(string(currentPath)) {
			return currentPath, nil, ErrLinkCycleDetected
		}

		if !currentNode.IsLink() {
			// no resolution and there is no next link (pseudo dead link)... return what you found
			// any content fetches will fail, but that's ok
			break
		}

		// prepare for the next iteration
		alreadySeen.Add(string(currentPath))

		var nextPath file.Path
		if currentNode.LinkPath.IsAbsolutePath() {
			// use links with absolute paths blindly
			nextPath = currentNode.LinkPath
		} else {
			// resolve relative link paths
			var parentDir string
			parentDir, _ = filepath.Split(string(currentNode.RealPath))
			// assemble relative link path by normalizing: "/cur/dir/../file1.txt" --> "/cur/file1.txt"
			nextPath = file.Path(filepath.Clean(path.Join(parentDir, string(currentNode.LinkPath))))
		}

		// no more links to follow
		if string(nextPath) == "" {
			break
		}

		// preserve the current node for the next loop (in case we shouldn't follow a potentially dead link)
		lastNode = currentNode
		lastPath = currentPath

		// get the next node (based on the next path)
		currentPath, currentNode, err = t.resolveAncestorLinks(nextPath)
		if err != nil {
			// only expected to occur upon cycle detection
			return currentPath, currentNode, err
		}
	}

	if currentNode == nil && !followDeadBasenameLinks {
		return lastPath, lastNode, nil
	}

	return currentPath, currentNode, nil
}

// File fetches zero to many file.References for the given glob pattern (considers symlinks).
func (t *FileTree) FilesByGlob(query string, options ...LinkResolutionOption) ([]GlobResult, error) {
	results := make([]GlobResult, 0)

	if len(query) == 0 {
		return nil, fmt.Errorf("no glob pattern given")
	}

	if query[0] != file.DirSeparator[0] {
		// this is for an image, so it should always be relative to root
		query = file.DirSeparator + query
	}

	doNotFollowDeadBasenameLinks := false
	for _, o := range options {
		if o == DoNotFollowDeadBasenameLinks {
			doNotFollowDeadBasenameLinks = true
		}
	}

	matches, err := doublestar.GlobOS(&osAdapter{
		filetree:                     t,
		doNotFollowDeadBasenameLinks: doNotFollowDeadBasenameLinks,
	}, query)
	if err != nil {
		return nil, err
	}

	for _, match := range matches {
		matchPath := file.Path(match)
		_, fn, err := t.node(matchPath, linkResolutionStrategy{
			FollowAncestorLinks:          true,
			FollowBasenameLinks:          true,
			DoNotFollowDeadBasenameLinks: doNotFollowDeadBasenameLinks,
		})
		if err != nil {
			return nil, err
		}
		// the node must exist and should not be a directory
		if fn != nil && fn.FileType != file.TypeDir {
			result := GlobResult{
				MatchPath: matchPath,
				RealPath:  fn.RealPath,
				// we should not be given a link node UNLESS it is dead
				IsDeadLink: fn.IsLink(),
			}
			if fn.Reference != nil {
				result.Reference = *fn.Reference
			}
			results = append(results, result)
		}
	}

	return results, nil
}

// AddFile adds a new path representing a REGULAR file to the Tree. It also adds any ancestors of the path that are not already
// present in the Tree. The resulting file.Reference of the new (leaf) addition is returned. Note: NO symlink or
// hardlink resolution is performed on the given path --which implies that the given path MUST be a real path (have no
// links in constituent paths)
func (t *FileTree) AddFile(realPath file.Path) (*file.Reference, error) {
	_, fn, err := t.node(realPath, linkResolutionStrategy{})
	if err != nil {
		return nil, err
	}
	if fn != nil {
		// this path already exists
		if fn.FileType != file.TypeReg {
			return nil, fmt.Errorf("path=%q already exists but is NOT a regular file", realPath)
		}
		// this is a regular file, provide a new or existing file.Reference
		if fn.Reference == nil {
			fn.Reference = file.NewFileReference(realPath)
		}
		return fn.Reference, nil
	}

	// this is a new path... add the new node + parents
	if err := t.addParentPaths(realPath); err != nil {
		return nil, err
	}
	newFn := filenode.NewFile(realPath, file.NewFileReference(realPath))
	return newFn.Reference, t.setFileNode(newFn)
}

// AddSymLink adds a new path to the Tree that represents a SYMLINK. A new file.Reference with a absolute or relative
// link path captured and returned. Note: NO symlink or hardlink resolution is performed on the given path --which
// implies that the given path MUST be a real path (have no links in constituent paths)
func (t *FileTree) AddSymLink(realPath file.Path, linkPath file.Path) (*file.Reference, error) {
	_, fn, err := t.node(realPath, linkResolutionStrategy{})
	if err != nil {
		return nil, err
	}
	if fn != nil {
		// this path already exists
		if fn.FileType != file.TypeSymlink {
			return nil, fmt.Errorf("path=%q already exists but is NOT a symlink file", realPath)
		}
		// this is a symlink file, provide a new or existing file.Reference
		if fn.Reference == nil {
			fn.Reference = file.NewFileReference(realPath)
		}
		return fn.Reference, nil
	}

	// this is a new path... add the new node + parents
	if err := t.addParentPaths(realPath); err != nil {
		return nil, err
	}
	newFn := filenode.NewSymLink(realPath, linkPath, file.NewFileReference(realPath))
	return newFn.Reference, t.setFileNode(newFn)
}

// AddHardLink adds a new path to the Tree that represents a HARDLINK. A new file.Reference with a absolute link
// path captured and returned. Note: NO symlink or hardlink resolution is performed on the given path --which
// implies that the given path MUST be a real path (have no links in constituent paths)
func (t *FileTree) AddHardLink(realPath file.Path, linkPath file.Path) (*file.Reference, error) {
	_, fn, err := t.node(realPath, linkResolutionStrategy{})
	if err != nil {
		return nil, err
	}
	if fn != nil {
		// this path already exists
		if fn.FileType != file.TypeHardLink {
			return nil, fmt.Errorf("path=%q already exists but is NOT a symlink file", realPath)
		}
		// this is a symlink file, provide a new or existing file.Reference
		if fn.Reference == nil {
			fn.Reference = file.NewFileReference(realPath)
		}
		return fn.Reference, nil
	}

	// this is a new path... add the new node + parents
	if err := t.addParentPaths(realPath); err != nil {
		return nil, err
	}

	newFn := filenode.NewHardLink(realPath, linkPath, file.NewFileReference(realPath))

	return newFn.Reference, t.setFileNode(newFn)
}

// AddDir adds a new path representing a DIRECTORY to the Tree. It also adds any ancestors of the path that are
// not already present in the Tree. The resulting file.Reference of the new (leaf) addition is returned.
// Note: NO symlink or hardlink resolution is performed on the given path --which implies that the given path MUST
// be a real path (have no links in constituent paths)
func (t *FileTree) AddDir(realPath file.Path) (*file.Reference, error) {
	_, fn, err := t.node(realPath, linkResolutionStrategy{})
	if err != nil {
		return nil, err
	}
	if fn != nil {
		// this path already exists
		if fn.FileType != file.TypeDir {
			return nil, fmt.Errorf("path=%q already exists but is NOT a symlink file", realPath)
		}
		// this is a symlink file, provide a new or existing file.Reference
		if fn.Reference == nil {
			fn.Reference = file.NewFileReference(realPath)
		}
		return fn.Reference, nil
	}

	// this is a new path... add the new node + parents
	if err := t.addParentPaths(realPath); err != nil {
		return nil, err
	}

	newFn := filenode.NewDir(realPath, file.NewFileReference(realPath))
	return newFn.Reference, t.setFileNode(newFn)
}

// addParentPaths adds paths into the Tree for all constituent paths, but does NOT attach a file.Reference for each new path.
// if the parent already exists, nothing is done and the function returns with no error. Note: NO symlink or hardlink
// resolution is performed on the given path --which implies that the given path MUST be a real path (have no
// links in constituent paths)
func (t *FileTree) addParentPaths(realPath file.Path) error {
	parentPath, err := realPath.ParentPath()
	if err != nil {
		return fmt.Errorf("unable to determine parent path while adding path=%q: %w", realPath, err)
	}

	_, fn, err := t.node(parentPath, linkResolutionStrategy{})
	if err != nil {
		return err
	}

	if fn == nil {
		// add parents of the node until an existent parent is found it's important to do this in reverse order
		// to ensure we are checking the fewest amount of parents possible.
		var pathsToAdd []file.Path
		parentPaths := realPath.ConstituentPaths()
		for idx := len(parentPaths) - 1; idx >= 0; idx-- {
			_, fn, err := t.node(parentPaths[idx], linkResolutionStrategy{})
			if err != nil {
				return err
			}
			if fn != nil {
				break
			}
			pathsToAdd = append(pathsToAdd, parentPaths[idx])
		}

		// add each path with no file reference; add these in sorted path order (which is guaranteed to be
		// the reverse of the order of insertion)
		for idx := len(pathsToAdd) - 1; idx >= 0; idx-- {
			newFn := filenode.NewDir(pathsToAdd[idx], nil)
			if err = t.setFileNode(newFn); err != nil {
				return err
			}
		}
	}
	return nil
}

// setFileNode adds the given path to the Tree with the specific file.Reference.
func (t *FileTree) setFileNode(fn *filenode.FileNode) error {
	if fn == nil {
		return fmt.Errorf("must provide a FileNode when adding paths")
	}

	if existingNode := t.tree.Node(filenode.IDByPath(fn.RealPath)); existingNode != nil {
		return t.tree.Replace(existingNode, fn)
	}

	parentPath, err := fn.RealPath.ParentPath()
	if err != nil {
		return fmt.Errorf("unable to determine parent path while adding path=%q: %w", fn.RealPath, err)
	}

	_, parentNode, err := t.node(parentPath, linkResolutionStrategy{})
	if err != nil {
		return err
	}
	if parentNode == nil {
		return fmt.Errorf("unable to find parent path=%q while adding path=%q", parentPath, fn.RealPath)
	}

	return t.tree.AddChild(parentNode, fn)
}

// RemovePath deletes the file.Reference from the FileTree by the given path. If the basename of the given path
// is a symlink then the symlink is removed (not the destination of the symlink). If the path does not exist, this is a
// nop.
func (t *FileTree) RemovePath(path file.Path) error {
	if path.Normalize() == "/" {
		return ErrRemovingRoot
	}

	_, fn, err := t.node(path, linkResolutionStrategy{
		FollowAncestorLinks: true,
		FollowBasenameLinks: false,
	})
	if err != nil {
		return err
	}
	if fn == nil {
		return nil
	}

	_, err = t.tree.RemoveNode(fn)
	if err != nil {
		return err
	}
	return nil
}

// RemoveChildPaths deletes all children of the given path (not including the given path). Note: if the given path
// basename is a symlink, then the symlink is followed before resolving children. If the path does not exist, this is a
// nop.
func (t *FileTree) RemoveChildPaths(path file.Path) error {
	_, fn, err := t.node(path, linkResolutionStrategy{
		FollowAncestorLinks: true,
		FollowBasenameLinks: true,
	})
	if err != nil {
		return err
	}
	if fn == nil {
		// can't remove child paths for node that doesn't exist!
		return nil
	}
	for _, child := range t.tree.Children(fn) {
		_, err := t.tree.RemoveNode(child)
		if err != nil {
			return err
		}
	}
	return nil
}

// Reader returns a tree.Reader useful for Tree traversal.
func (t *FileTree) Reader() tree.Reader {
	return t.tree
}

// PathDiff shows the path differences between two trees (useful for testing)
func (t *FileTree) PathDiff(other *FileTree) (extra, missing []file.Path) {
	extra = make([]file.Path, 0)
	missing = make([]file.Path, 0)

	ourPaths := internal.NewStringSet()
	for _, fn := range t.tree.Nodes() {
		ourPaths.Add(string(fn.ID()))
	}

	theirPaths := internal.NewStringSet()
	for _, fn := range other.tree.Nodes() {
		theirPaths.Add(string(fn.ID()))
	}

	for _, fn := range other.tree.Nodes() {
		if !ourPaths.Contains(string(fn.ID())) {
			extra = append(extra, file.Path(fn.ID()))
		}
	}

	for _, fn := range t.tree.Nodes() {
		if !theirPaths.Contains(string(fn.ID())) {
			missing = append(missing, file.Path(fn.ID()))
		}
	}

	return
}

// Equal indicates if the two trees have the same paths or not.
func (t *FileTree) Equal(other *FileTree) bool {
	if t.tree.Length() != other.tree.Length() {
		return false
	}

	extra, missing := t.PathDiff(other)

	return len(extra) == 0 && len(missing) == 0
}

// HasPath indicates is the given path is in the file Tree.
func (t *FileTree) HasPath(path file.Path) bool {
	exists, _, err := t.File(path, FollowBasenameLinks)
	if err != nil {
		return false
	}
	return exists
}

// TODO: critical! are these still needed? try out basic.go before deleting them...

//// fileByPathID indicates if the given node ID is in the FileTree (useful for Tree -> FileTree node resolution).
//func (t *FileTree) fileByPathID(id node.ID) *file.Reference {
//	return t.tree.Node(id).(*filenode.FileNode).Reference
//}
//
//// VisitorFn, used for traversal, wraps the given user function (meant to walk file.References) with a function that
//// can resolve an underlying tree Node to a file.Reference.
//func (t *FileTree) VisitorFn(fn func(file.Reference)) func(node.Node) {
//	return func(node node.Node) {
//		fn(t.fileByPathID(node.ID()))
//	}
//}
//
//// ConditionFn, used for conditioning traversal, wraps the given user function (meant to walk file.References) with a
//// function that can resolve an underlying tree Node to a file.Reference.
//func (t *FileTree) ConditionFn(fn func(file.Reference) bool) func(node.Node) bool {
//	return func(node node.Node) bool {
//		return fn(t.fileByPathID(node.ID()))
//	}
//}

// Walk takes a visitor function and invokes it for all paths within the FileTree in depth-first ordering.
func (t *FileTree) Walk(fn func(path file.Path, f filenode.FileNode) error) error {
	return NewDepthFirstPathWalker(t, fn, nil).WalkAll()
}

// merge takes the given Tree and combines it with the current Tree, preferring files in the other Tree if there
// are path conflicts. This is the basis function for squashing (where the current Tree is the bottom Tree and the
// given Tree is the top Tree).
// nolint:gocognit
func (t *FileTree) merge(upper *FileTree) error {
	conditions := tree.WalkConditions{
		ShouldContinueBranch: func(n node.Node) bool {
			p := file.Path(n.ID())
			return !p.IsWhiteout()
		},
		ShouldVisit: func(n node.Node) bool {
			p := file.Path(n.ID())
			return !p.IsDirWhiteout()
		},
	}

	visitor := func(n node.Node) error {
		if n == nil {
			return fmt.Errorf("found nil node while traversing %+v", upper)
		}
		upperNode := n.(*filenode.FileNode)
		// opaque directories must be processed first
		if upper.hasOpaqueDirectory(upperNode.RealPath) {
			err := t.RemoveChildPaths(upperNode.RealPath)
			if err != nil {
				return fmt.Errorf("filetree merge failed to remove child paths (upperPath=%s): %w", upperNode.RealPath, err)
			}
		}

		if upperNode.RealPath.IsWhiteout() {
			lowerPath, err := upperNode.RealPath.UnWhiteoutPath()
			if err != nil {
				return fmt.Errorf("filetree merge failed to find original upperPath for whiteout (upperPath=%s): %w", upperNode.RealPath, err)
			}

			err = t.RemovePath(lowerPath)
			if err != nil {
				return fmt.Errorf("filetree merge failed to remove upperPath (upperPath=%s): %w", lowerPath, err)
			}

			return nil
		}

		_, originalNode, err := t.node(upperNode.RealPath, linkResolutionStrategy{
			FollowAncestorLinks: false,
			FollowBasenameLinks: false,
		})
		if err != nil {
			return fmt.Errorf("filetree merge failed when looking for path=%q : %w", upperNode.RealPath, err)
		}
		if originalNode == nil {
			// there is no existing node... add parents and prepare to set
			if err := t.addParentPaths(upperNode.RealPath); err != nil {
				return fmt.Errorf("could not add parent paths to lower: %w", err)
			}
		}

		nodeCopy := *upperNode

		// keep original file references if the upper tree does not have them (only for the same file types)
		if originalNode != nil && originalNode.Reference != nil && upperNode.Reference == nil && upperNode.FileType == originalNode.FileType {
			nodeCopy.Reference = originalNode.Reference
		}

		// graft a copy of the upper node with potential lower information into the lower tree
		if err := t.setFileNode(&nodeCopy); err != nil {
			return fmt.Errorf("filetree merge failed to set file node (node=%+v): %w", nodeCopy, err)
		}

		return nil
	}

	// we are using the tree walker instead of the path walker to only look at an resolve merging of real files
	// with no consideration to virtual paths (paths that are valid in the filetree because constituent paths
	// contain symlinks).
	return tree.NewDepthFirstWalkerWithConditions(upper.Reader(), visitor, conditions).WalkAll()
}

func (t *FileTree) hasOpaqueDirectory(directoryPath file.Path) bool {
	opaqueWhiteoutChild := file.Path(path.Join(string(directoryPath), file.OpaqueWhiteout))
	return t.HasPath(opaqueWhiteoutChild)
}

//
//func mustMatch(path file.Path, ref *file.Reference) error {
//	if ref != nil && filenode.IDByPath(path) != filenode.IDByPath(ref.RealPath) {
//		return fmt.Errorf("unable to add path for mismatched reference value: %+v != %+v", path, ref.RealPath)
//	}
//	return nil
//}
