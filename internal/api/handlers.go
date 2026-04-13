package api

// API route plan:
//
// POST   /api/projects           - add project
// GET    /api/projects           - list projects
// GET    /api/projects/:name     - get project
// PUT    /api/projects/:name     - update project
// DELETE /api/projects/:name     - delete project
//
// POST   /api/jobs               - submit job (from JobConfig JSON)
// GET    /api/jobs               - list jobs
// GET    /api/jobs/:id           - get job + tasks
// DELETE /api/jobs/:id           - kill job
// POST   /api/jobs/:id/pause     - pause job
// POST   /api/jobs/:id/resume    - resume job
//
// GET    /api/tasks              - list tasks (with filters)
// GET    /api/tasks/:id          - get task detail
// POST   /api/tasks/:id/kill     - kill task
// POST   /api/tasks/:id/retry    - retry task
// GET    /api/tasks/:id/logs     - stream task log
//
// GET    /api/gpu                - GPU status
// GET    /api/status             - daemon status
//
// TODO: implement handlers
