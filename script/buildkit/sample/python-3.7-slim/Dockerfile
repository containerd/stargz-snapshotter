#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

FROM ktokunaga/python:3.7-slim-{{ IMAGE_MODE_SUFFIX }} AS dev
COPY ./app /app
RUN cd /app && pip install --user --install-option="--no-compile" .

FROM ktokunaga/python:3.7-slim-org
COPY --from=dev /root/.local /root/.local

ENTRYPOINT [ "python" ]
CMD [ "/root/.local/bin/hello.py" ]
