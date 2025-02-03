using Microsoft.Extensions.ML;
using prediction.Server.Hubs;
using Prediction_Server;

var builder = WebApplication.CreateBuilder(args);

builder.AddServiceDefaults();

// Add services to the container.
builder.Services.AddSignalR();

builder.Services.AddControllers();
// Learn more about configuring OpenAPI at https://aka.ms/aspnet/openapi
builder.Services.AddOpenApi();
builder.Services.AddPredictionEnginePool<PredictionModel.ModelInput, PredictionModel.ModelOutput>()
    .FromFile("PredictionModel.mlnet");
builder.Services.AddCors(options =>
{
    options.AddPolicy("AllowAllOrigins", policy =>
{
    policy.AllowAnyOrigin().AllowAnyHeader().AllowAnyMethod();
});

});


var app = builder.Build();
app.MapHub<AccessControlHub>("/accessControlHub");

app.MapDefaultEndpoints();

app.UseDefaultFiles();
app.MapStaticAssets();

// Configure the HTTP request pipeline.
if (app.Environment.IsDevelopment())
{
    app.MapOpenApi();
}
app.UseCors("AllowAllOrigins");

app.UseHttpsRedirection();

app.UseAuthorization();

app.MapControllers();

app.MapFallbackToFile("/index.html");

app.Run();
