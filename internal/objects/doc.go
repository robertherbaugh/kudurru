// Package objects loads file-managed kudurru objects (docs/architecture.md
// §6.2): the CRD-shaped documents embedded under objects: in kudurru.yaml
// and every *.yaml file in objectsDir (default /etc/kudurru/objects.d).
//
// A Loader reconciles them into the store with the Apply contract of §4.3
// (decision D9):
//
//   - every document is decoded as strict YAML, validated with
//     validator.ValidateUnstructured (unknown fields rejected, schema
//     defaults applied, CEL rules run), converted to its api/v1alpha1
//     type and validated once more as the typed write the driver sees;
//   - it is applied with Manager "file:<source>", where source is
//     "kudurru.yaml" for inline objects and "<base(objectsDir)>/<file>"
//     (e.g. "objects.d/zones.yaml") for files, so the UI can say "managed
//     by file objects.d/zones.yaml";
//   - the ownership ledger is the store itself: an object whose managedBy
//     starts with "file:" and that no source declares any more is deleted,
//     so deleting a document (or its file) deletes the object;
//   - nothing aborts a load: invalid YAML, unknown kinds or fields, invalid
//     objects, duplicate declarations and objects owned by the API or an
//     integration each become a Diagnosis while the rest is applied, and
//     a source that cannot be read or parsed completely keeps the objects
//     it owns (last good state);
//   - first-boot defaults (managedBy "bootstrap", see internal/bootstrap)
//     are taken over by a file that declares the same object, and an
//     object that moves between files changes owner without being
//     deleted.
//
// Watch re-runs the load on fsnotify events (debounced) and on a poll
// interval that also repairs store drift. ApplyFile is the one-shot helper
// behind `kudurru apply -f` (manager "api", nothing deleted).
package objects
