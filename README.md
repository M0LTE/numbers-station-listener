# Numbers Station Listener

A website that shows which shortwave numbers stations are on air now and next, from the [Priyom](https://priyom.org) schedule, picks the best public [UberSDR](https://github.com/madpsy/ka9q_ubersdr) receiver for each transmission, and plays it in the page with audio, an RF waterfall and a spectrogram.

Work in progress. See [docs/brief.md](docs/brief.md) for the design brief and [docs/api.md](docs/api.md) for the HTTP API.

## Licence

Copyright 2026 Tom Fanning (M0LTE).

This program is free software: you can redistribute it and/or modify it under the terms of the GNU Affero General Public License as published by the Free Software Foundation, either version 3 of the License, or (at your option) any later version. See [LICENSE](LICENSE).

Schedule and station information comes from [Priyom.org](https://priyom.org) under [CC BY-NC-SA 4.0](https://creativecommons.org/licenses/by-nc-sa/4.0/); the station catalogue in `data/` carries that licence. Audio comes from volunteer-run UberSDR receivers, credited wherever they are heard.
