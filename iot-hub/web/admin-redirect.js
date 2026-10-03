// The old settings page (admin.html#devices) is now part of the app.
location.replace('/#/' + (location.hash.slice(1).replace(/^\//, '') || 'sensors'));
