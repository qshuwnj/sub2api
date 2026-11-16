# Dapacai video downloads

Completed Dapacai tasks are retrieved with an authenticated GET to
`/v1/videos/{task_id}/content`. The response may redirect to either of the
verified task-specific destinations:

- `https://dapacaihk-media.oss-cn-hongkong.aliyuncs.com/video/YYYY/MM/{task_id}.mp4`
- `https://media.dapacai.com/dapacai-media/video/YYYY/MM/{task_id}.mp4`

The gateway validates the exact HTTPS host, path and task ID before following
one redirect. Download requests do not include supplier credentials, caller
cookies or caller authorization. Private DNS destinations and further redirects
remain blocked. Range requests are preserved. The same validation is used for
direct downloads and native video archival.

Recover an already completed task by querying its existing task ID and retrieving
its content. Do not submit another generation request to recover a download
failure. Canvas recovery must use the original billing order and must not charge
an already settled order again.
